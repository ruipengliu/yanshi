// Package paging 给应用查询提供有界分页；游标只表示位置，不能替代 owner 鉴权。
package paging

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
)

var ErrInvalid = errors.New("invalid pagination")

const MaxLimit = 100

func Limit(values url.Values) (int, error) {
	if !values.Has("limit") {
		return 30, nil
	}
	n, err := strconv.Atoi(values.Get("limit"))
	if err != nil || n < 1 || n > MaxLimit {
		return 0, ErrInvalid
	}
	return n, nil
}

// Scope 绑定 owner、资源和筛选条件，防止一个页面的位置被误用于另一个查询。
func Scope(parts ...string) string {
	data, _ := json.Marshal(parts)
	hash := sha256.Sum256(data)
	return base64.RawURLEncoding.EncodeToString(hash[:])
}

func Encode(scope, key string) string {
	data, _ := json.Marshal([3]string{"1", scope, key})
	return base64.RawURLEncoding.EncodeToString(data)
}

func Decode(cursor, scope string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	if len(cursor) > 4096 {
		return "", ErrInvalid
	}
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	var fields []string
	if err != nil || json.Unmarshal(data, &fields) != nil || len(fields) != 3 || fields[0] != "1" || fields[1] != scope || fields[2] == "" {
		return "", ErrInvalid
	}
	return fields[2], nil
}

type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// Slice 使用排序键而非数组下标，删除上一页的末项也不改变后续位置。
func Slice[T any](items []T, limit int, after, scope string, key func(T) string) Page[T] {
	out := Page[T]{Items: []T{}}
	for _, item := range items {
		if key(item) <= after {
			continue
		}
		if len(out.Items) == limit {
			out.NextCursor = Encode(scope, key(out.Items[len(out.Items)-1]))
			break
		}
		out.Items = append(out.Items, item)
	}
	return out
}
