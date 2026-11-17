package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/devyunmu/iot-alert-agent/internal/domain"
)

// Mock is a deterministic rule-based analyzer used when no LLM endpoint is
// configured. It exists so the demo and the eval runner still exercise the full
// pipeline offline; it is never presented as model output in the UI.
type Mock struct{}

// NewMock returns the offline analyzer.
func NewMock() *Mock { return &Mock{} }

// Describe identifies the analyzer in reports.
func (m *Mock) Describe() string { return "mock://rules-v1" }

// Mock reports true so callers can label results honestly.
func (m *Mock) Mock() bool { return true }

// Analyze renders a rule-based triage result. Numbers and times come from the
// input record, so the fallback cannot fabricate telemetry.
func (m *Mock) Analyze(_ context.Context, alert domain.Alert, device domain.Device) (domain.Analysis, error) {
	start := time.Now()
	a := domain.Analysis{
		AlertID:        alert.ID,
		Summary:        clampRunes(alert.Title+"："+alert.Message, 220),
		Severity:       alert.Severity,
		SeverityReason: mockSeverityReason(alert),
		Model:          m.Describe(),
		Mock:           true,
		Confidence:     0.55,
		GeneratedAt:    time.Now().UTC(),
		LatencyMS:      time.Since(start).Milliseconds(),
	}

	switch alert.Type {
	case domain.TypeOffline:
		a.LikelyCauses = []string{
			"设备供电中断或电源适配器故障",
			"上行网络/网关链路异常，或设备所在基站信号弱",
			"设备固件异常重启后未能重新入网",
		}
		a.Actions = []string{
			"核对设备最后心跳时间与同网关其他设备状态，判断是单点离线还是汇聚链路故障",
			"检查设备供电、网线或天线连接，必要时远程重启并观察 5 分钟心跳",
			"30 分钟内未恢复则派单现场处理，并升级至值班主管",
		}
	case domain.TypeLowBattery:
		a.LikelyCauses = []string{
			"电池自然衰减达到寿命末期",
			"低温环境导致电池有效容量下降",
			"设备唤醒/上报过于频繁，耗电高于预期",
		}
		a.Actions = []string{
			"核对电池电压与近 7 日耗电曲线，确认是否存在异常掉电",
			"24 小时内安排更换电池，消防、冷链等关键设备优先",
			"核对备件库存并发起更换工单，更换后验证电量与心跳上报",
		}
	case domain.TypeSensorAnomaly:
		a.LikelyCauses, a.Actions = sensorRules(alert)
	case domain.TypeOTAFailure:
		a.LikelyCauses = []string{
			"固件包损坏或签名/校验和不匹配",
			"设备电量、存储空间或网络质量不满足升级前置条件",
			"升级过程中断导致回滚失败",
		}
		a.Actions = []string{
			"核对目标固件包的哈希与签名，确认发布批次完整",
			"检查设备电量、可用存储与网络质量是否达到升级要求",
			"清除失败任务后重试一次；仍失败则保持回滚版本并记录错误码",
			"将该批次移出灰度范围，修复发布包后再重新推送",
		}
	case domain.TypeCommFlap:
		a.LikelyCauses = []string{
			"现场信号弱或运营商网络抖动",
			"上联端口/网线接触不良，存在丢包",
			"供电波动或电磁干扰导致设备反复重启",
		}
		a.Actions = []string{
			"查看设备信号强度、丢包率与上联端口错包计数",
			"检查供电稳定性、天线接触与现场电磁干扰源",
			"持续观察 1 小时；抖动未收敛则安排现场巡检并考虑更换信道",
		}
	default:
		a.LikelyCauses = []string{"告警类型未匹配到处置规则"}
		a.Actions = []string{"转人工复核告警内容与设备档案"}
	}
	return a, nil
}

func mockSeverityReason(a domain.Alert) string {
	switch a.Severity {
	case domain.SeverityCritical:
		return fmt.Sprintf("%s 影响核心业务或安全，基线定级为严重，需要立即处置", a.Type.Label())
	case domain.SeverityWarning:
		return fmt.Sprintf("%s 需要人工跟进，但暂未影响核心业务连续性", a.Type.Label())
	default:
		return fmt.Sprintf("%s 属于提示类事件，按常规巡检处理", a.Type.Label())
	}
}

// sensorRules picks a metric-specific runbook. This is what makes the offline
// fallback useful rather than generic: a smoke alarm and a pH excursion need very
// different first actions.
func sensorRules(a domain.Alert) (causes, actions []string) {
	switch a.Metric {
	case "smoke_obscuration":
		return []string{
			"现场存在真实烟雾或明火",
			"探头积尘、水汽导致误报",
			"周边动火作业或扬尘进入探测器",
		}, []string{
			"立即通知消防值班与现场安保核实火情，必要时启动疏散",
			"确认非火情后检查探头清洁度并复位，记录误报原因",
			"保持通风并切断相关区域非必要电源，持续观察浓度变化",
		}
	case "current_a":
		return []string{
			"负载侧设备集中启动或新增负载，导致电流过载",
			"线路接触不良或局部短路",
			"电流互感器/采集通道存在偏差",
		}, []string{
			"核对配电回路负载并降载，避免长时间过载运行",
			"检查断路器、接线端子与电缆温度，确认保护定值有效",
			"用钳形表复测电流，偏差较大时校验采集器",
		}
	case "ph":
		return []string{
			"加药系统投加过量或搅拌不均",
			"PH 探头污染、老化或未按期校准",
			"上游来水水质波动",
		}, []string{
			"与实验室取样结果比对，确认 PH 9.4 是否真实越限",
			"清洁并校准 PH 探头，检查最近标定记录",
			"调整加药量，关注下游生化池活性污泥状态与排放合规",
		}
	case "level_m":
		return []string{
			"补水阀未开启或补水系统故障",
			"水池/管道存在泄漏",
			"液位探头卡滞或零点漂移",
		}, []string{
			"立即核查消防水池实际液位，确认是否低于 0.80m 下限",
			"开启补水阀注水，检查补水系统压力与流量",
			"校验液位探头零点，排查池体与管道泄漏，恢复消防供水能力",
		}
	case "door_open_min":
		return []string{
			"库门未关闭或门体被货物阻挡",
			"门磁安装位置偏移或磁体脱落",
			"人员频繁进出未及时关门",
		}, []string{
			"现场关闭库门并检查门封、风幕是否完好",
			"核对门磁与磁体间距，复位后观察上报状态",
			"提醒库管人员执行随手关门，评估库温与能耗影响",
		}
	case "turbidity_ntu":
		return []string{
			"过滤单元堵塞或反冲洗不及时",
			"上游来水悬浮物升高",
			"浊度探头污染",
		}, []string{
			"复测出水浊度并与实验室数据比对",
			"检查过滤与反冲洗运行状态，必要时切换备用支路",
			"清洁浊度探头，确认排放指标恢复至 5 NTU 以下",
		}
	default: // temperature_c, temp_deviation_c and unknown metrics
		return []string{
			"制冷机组/压缩机效率下降或停机",
			"库门长时间开启、门封失效导致冷量流失",
			"温度探头结霜、漂移或安装位置不当",
		}, []string{
			"与同区域冗余传感器及人工测温结果比对，排除单点漂移",
			"检查制冷机组、蒸发器结霜与除霜周期，确认制冷能力",
			"核查库门、门封与风幕状态，评估货品冻损或升温风险并持续观察",
		}
	}
}
