package pgstore

import (
	"encoding/json"
	"fmt"

	"persona/domain"
)

type bookParts map[string]json.RawMessage

func splitBook(book *domain.Book) (bookParts, error) {
	data, err := json.Marshal(book)
	if err != nil {
		return nil, err
	}
	var parts bookParts
	if err := json.Unmarshal(data, &parts); err != nil {
		return nil, err
	}
	meta := bookParts{}
	for _, field := range []string{"Next", "NextC", "NextCS"} {
		meta[field] = parts[field]
		delete(parts, field)
	}
	if parts["Meta"], err = json.Marshal(meta); err != nil {
		return nil, err
	}
	var days map[string]bookParts
	if err := json.Unmarshal(parts["CaptureDays"], &days); err != nil {
		return nil, err
	}
	tombstones := bookParts{}
	for date, day := range days {
		tombstones[date] = day["Tombstones"]
		if string(day["Tombstones"]) == "null" {
			day["Tombstones"] = json.RawMessage("null")
		} else {
			day["Tombstones"] = json.RawMessage("[]")
		}
	}
	if parts["CaptureDays"], err = json.Marshal(days); err != nil {
		return nil, err
	}
	if parts["Tombstones"], err = json.Marshal(tombstones); err != nil {
		return nil, err
	}
	// 保存物化供后续产品查询，但恢复继续调用领域的 refold，不另实现叠加规则。
	state := book.State()
	if parts["Exchanges"], err = json.Marshal(state.Exchanges); err != nil {
		return nil, err
	}
	if parts["Commitments"], err = json.Marshal(state.Commitments); err != nil {
		return nil, err
	}
	if len(parts) != len(categories) {
		return nil, fmt.Errorf("persona book storage fields changed; update category mapping")
	}
	for _, c := range categories {
		if _, ok := parts[c.field]; !ok {
			return nil, fmt.Errorf("persona book storage field missing: %s", c.field)
		}
	}
	return parts, nil
}

func joinBook(parts bookParts) (*domain.Book, error) {
	var meta bookParts
	if err := json.Unmarshal(parts["Meta"], &meta); err != nil {
		return nil, err
	}
	delete(parts, "Meta")
	for field, value := range meta {
		parts[field] = value
	}
	var days map[string]bookParts
	if err := json.Unmarshal(parts["CaptureDays"], &days); err != nil {
		return nil, err
	}
	var tombstones bookParts
	if err := json.Unmarshal(parts["Tombstones"], &tombstones); err != nil {
		return nil, err
	}
	for date, day := range days {
		value, ok := tombstones[date]
		if !ok {
			return nil, fmt.Errorf("persona business day is missing tombstones")
		}
		day["Tombstones"] = value
	}
	var err error
	if parts["CaptureDays"], err = json.Marshal(days); err != nil {
		return nil, err
	}
	delete(parts, "Tombstones")
	delete(parts, "Exchanges")
	delete(parts, "Commitments")
	data, err := json.Marshal(parts)
	if err != nil {
		return nil, err
	}
	var book domain.Book
	if err := json.Unmarshal(data, &book); err != nil {
		return nil, err
	}
	return &book, nil
}
