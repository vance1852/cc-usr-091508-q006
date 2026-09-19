package store

import "time"

// User 是系统内的操作者（采样员、化验员、复核人、审批人等）。
type User struct {
	ID          int64
	Token       string
	Login       string
	DisplayName string
	Role        string
	CrewCode    string
	Active      bool
}

// Batch 为试车批（如某次发动机试车的液氧批）。
type Batch struct {
	ID        int64  `json:"id"`
	Code      string `json:"code"`
	Product   string `json:"product"`
	CreatedAt string `json:"created_at"`
	CreatedBy int64  `json:"created_by"`
}

// SamplingPoint 为采样点（储罐、加注管路等）。
type SamplingPoint struct {
	ID   int64  `json:"id"`
	Code string `json:"code"`
	Name string `json:"name"`
}

// Method 为检测方法，携带该方法要求的运输温度区间与最大记录间隔（分钟）。
type Method struct {
	ID            int64    `json:"id"`
	Code          string   `json:"code"`
	Name          string   `json:"name"`
	Standard      string   `json:"standard,omitempty"`
	Unit          string   `json:"unit,omitempty"`
	TempMin       *float64 `json:"temp_min,omitempty"`
	TempMax       *float64 `json:"temp_max,omitempty"`
	MaxGapMinutes int      `json:"max_gap_minutes,omitempty"`
}

// Shipment 为一次转运箱发运登记。
type Shipment struct {
	ID        int64  `json:"id"`
	BoxCode   string `json:"box_code"`
	BatchID   int64  `json:"batch_id"`
	CrewCode  string `json:"crew_code"`
	CreatedAt string `json:"created_at"`
	ClosedAt  string `json:"closed_at,omitempty"`
}

// Container 为可重复使用的物理容器（采集瓶等）。
type Container struct {
	ID        int64  `json:"id"`
	Code      string `json:"code"`
	Kind      string `json:"kind"`
	CreatedAt string `json:"created_at"`
}

// Sample 为一个采集瓶样品及其当前链路状态。
type Sample struct {
	ID                 int64    `json:"id"`
	BatchID            int64    `json:"batch_id"`
	ShipmentID         int64    `json:"shipment_id"`
	LabNo              string   `json:"lab_no"`
	PointCode          string   `json:"point_code"`
	MethodCode         string   `json:"method_code"`
	ContainerCode      string   `json:"container_code"`
	SealNo             string   `json:"seal_no"`
	SampledAt          string   `json:"sampled_at"`
	CrewCode           string   `json:"crew_code"`
	RegisteredBy       int64    `json:"registered_by"`
	CreatedAt          string   `json:"created_at"`
	TempMin            *float64 `json:"temp_min,omitempty"`
	TempMax            *float64 `json:"temp_max,omitempty"`
	MaxGapMinutes      int      `json:"max_gap_minutes,omitempty"`
	CurrentCustodianID *int64   `json:"current_custodian_id"`
	CurrentCustodian   string   `json:"current_custodian,omitempty"`
	CustodianSeq       int      `json:"custodian_seq"`
	CurrentResultID    *int64   `json:"current_result_id"`
	OpenDeviationCount int      `json:"open_deviation_count"`
	Usable             bool     `json:"usable"`
	ConsumedAt         string   `json:"consumed_at,omitempty"`
}

// RegisterSampleInput 为登记样品的输入。
type RegisterSampleInput struct {
	BatchCode     string `json:"batch_code"`
	BoxCode       string `json:"box_code"`
	LabNo         string `json:"lab_no"`
	ContainerCode string `json:"container_code"`
	SealNo        string `json:"seal_no"`
	PointCode     string `json:"point_code"`
	MethodCode    string `json:"method_code"`
	SampledAt     string `json:"sampled_at"`
}

// CustodyEvent 为交接链路上的一个事实。
type CustodyEvent struct {
	ID              int64  `json:"id"`
	SampleID        int64  `json:"sample_id"`
	Seq             int    `json:"seq"`
	Kind            string `json:"kind"`
	Station         string `json:"station,omitempty"`
	FromUserID      *int64 `json:"from_user_id,omitempty"`
	ToUserID        *int64 `json:"to_user_id,omitempty"`
	FromUser        string `json:"from_user,omitempty"`
	ToUser          string `json:"to_user,omitempty"`
	ActorID         int64  `json:"actor_id"`
	SealIntact      *bool  `json:"seal_intact,omitempty"`
	ExpectedSealNo  string `json:"expected_seal_no,omitempty"`
	TransferID      *int64 `json:"transfer_id,omitempty"`
	ClientEventTime string `json:"client_event_time,omitempty"`
	IdemKey         string `json:"idem_key,omitempty"`
	Note            string `json:"note,omitempty"`
	RecordedAt      string `json:"recorded_at"`
	PrevHash        string `json:"-"`
	Hash            string `json:"hash"`
}

// TemperatureReading 为一次温度记录。
type TemperatureReading struct {
	ID         int64   `json:"id"`
	SampleID   int64   `json:"sample_id"`
	TakenAt    string  `json:"taken_at"`
	Celsius    float64 `json:"celsius"`
	Source     string  `json:"source"`
	RecordedBy int64   `json:"recorded_by"`
	RecordedAt string  `json:"recorded_at"`
}

// Deviation 为一次偏差（封签破损、温度越界、温度空白、编号冲突）。
type Deviation struct {
	ID          int64        `json:"id"`
	SampleID    int64        `json:"sample_id"`
	Type        string       `json:"type"`
	Detail      string       `json:"detail"`
	RefEventID  *int64       `json:"ref_event_id,omitempty"`
	AttemptID   *int64       `json:"attempt_id,omitempty"`
	OpenedBy    int64        `json:"opened_by"`
	OpenedAt    string       `json:"opened_at"`
	Open        bool         `json:"open"`
	Disposition *Disposition `json:"disposition,omitempty"`
}

// Disposition 为偏差处置（结案决定）。
type Disposition struct {
	ID            int64  `json:"id"`
	DeviationID   int64  `json:"deviation_id"`
	Decision      string `json:"decision"`
	Justification string `json:"justification"`
	ActorID       int64  `json:"actor_id"`
	DecidedAt     string `json:"decided_at"`
}

// ResultVersion 为检测结果的一个不可变版本。
type ResultVersion struct {
	ID           int64        `json:"id"`
	SampleID     int64        `json:"sample_id"`
	VersionNo    int          `json:"version_no"`
	MethodCode   string       `json:"method_code"`
	Instrument   string       `json:"instrument,omitempty"`
	AnalystID    int64        `json:"analyst_id"`
	Analyst      string       `json:"analyst"`
	SupersedesID *int64       `json:"supersedes_id,omitempty"`
	SubmittedAt  string       `json:"submitted_at"`
	Note         string       `json:"note,omitempty"`
	Status       string       `json:"status"`
	Conclusion   string       `json:"conclusion,omitempty"`
	Readings     []RawReading `json:"readings"`
}

// RawReading 为原始读数。
type RawReading struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	Value      float64 `json:"value"`
	Unit       string  `json:"unit,omitempty"`
	RecordedAt string  `json:"recorded_at"`
}

// parseTime 接受 RFC3339 时间，空串视为零值。
func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}
