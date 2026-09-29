package service

import (
	"time"

	"github.com/wyw14/solo-039/internal/column"
)

// ObserveColumnSignals 消费一次连续塔压/液位实测：刷新塔状态并推进分阶段包络状态机。
// 它替代"单点硬阈值即拒"的判定——许可由 normal/limiting/recovering/hold 迟滞状态给出，
// 恢复须经过连续稳定窗口。返回的状态包含当前限制的信号、原因与历史归因。
func (p *Plant) ObserveColumnSignals(pressure, level, temperature float64) (column.EnvelopeStatus, error) {
	current := p.Campaign.Snapshot()
	sample := column.Sample{At: time.Now().Unix(), Pressure: pressure, Level: level}
	if err := p.ColumnEnvelope.Observe(current, sample); err != nil {
		// 非数值等非法样本：不改变限制状态，也不写入塔状态。
		return p.ColumnEnvelope.Status(), err
	}
	// 状态机已接受该样本，同步到既有塔状态（含阶段/围栏）。
	p.Column.Apply(current, pressure, level)
	p.Column.SetTemperature(temperature)

	status := p.ColumnEnvelope.Status()
	switch status.State {
	case column.Hold:
		p.Incidents.Open("column-envelope", status.Reasons[0], current.Revision.String())
	case column.Limiting:
		// 限制不停车，但留下最近一次限制事件便于追溯。
		p.Incidents.Open("column-envelope-limit", status.Reasons[0], current.Revision.String())
	}
	return status, nil
}

// ColumnEnvelopeStatus 返回操作员可见的包络状态（哪项信号、什么原因、哪段历史导致当前限制）。
func (p *Plant) ColumnEnvelopeStatus() column.EnvelopeStatus {
	return p.ColumnEnvelope.Status()
}

// ColumnEnvelopeHistory 返回最近的连续压液实测历史。
func (p *Plant) ColumnEnvelopeHistory() []column.Sample {
	return p.ColumnEnvelope.History()
}

// HeatActionAllowed 依据当前包络状态给出增热/提量方向动作许可，
// 供 reboiler ramp、lease 等会抬升压/液风险的动作在执行前查询。
func (p *Plant) HeatActionAllowed() bool {
	return p.ColumnEnvelope.ActionAllowed("heat-up")
}

// seedEnvelope 用当前快照给包络监控播种（启动/恢复后），使首个限制判定有连续基线。
func (p *Plant) seedEnvelope() {
	current := p.Campaign.Snapshot()
	snap := p.Column.Snapshot()
	_ = p.ColumnEnvelope.Observe(current, column.Sample{
		At:       time.Now().Unix(),
		Pressure: snap.Pressure,
		Level:    snap.Level,
	})
}
