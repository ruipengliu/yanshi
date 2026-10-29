package pgstore

import (
	"bytes"
	"encoding/json"
)

// PostgreSQL 的 JSONB 不接受 U+0000，Go 的 JSON 与领域文字却允许。
// 类别载荷只可能是对象、数组或 null；仅这个边界值改用 JSON 字符串包住原 JSON，
// 读取时还原，既不拒收内存实现接受的文字，也不把大整数经过 float64。
func encodePayload(data []byte) string {
	if bytes.Contains(data, []byte(`\u0000`)) {
		encoded, _ := json.Marshal(string(data))
		return string(encoded)
	}
	return string(data)
}

func decodePayload(data []byte) ([]byte, error) {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '"' {
		var raw string
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, err
		}
		return []byte(raw), nil
	}
	return data, nil
}

func unmarshalPayload(data []byte, value any) error {
	raw, err := decodePayload(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, value)
}
