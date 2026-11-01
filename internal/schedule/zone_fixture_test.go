package schedule_test

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const midnightFallZone = "Fixture/MidnightFall"

// TestMain 用虚构时区固定午夜回拨，避免真实时区的规则随 tzdata 更新而改变。
// LoadLocation 首次读取后会缓存 ZONEINFO，因此必须在任何 LoadLocation 调用前设置。
// 目录中不存在的名称仍回落到系统时区库与 Go 时区数据，其他用例继续使用原有规则。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "schedule-zoneinfo-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "创建测试时区目录失败：", err)
		os.Exit(1)
	}
	code := func() int {
		data := midnightFallTZData()
		if _, err := time.LoadLocationFromTZData(midnightFallZone, data); err != nil {
			fmt.Fprintln(os.Stderr, "测试时区 TZif 自检失败：", err)
			return 1
		}
		path := filepath.Join(dir, filepath.FromSlash(midnightFallZone))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			fmt.Fprintln(os.Stderr, "创建测试时区子目录失败：", err)
			return 1
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			fmt.Fprintln(os.Stderr, "写入测试时区失败：", err)
			return 1
		}
		if err := os.Setenv("ZONEINFO", dir); err != nil {
			fmt.Fprintln(os.Stderr, "设置测试时区目录失败：", err)
			return 1
		}
		return m.Run()
	}()
	// os.Exit 不运行 defer，正常结束和初始化失败都必须先清理目录。
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, "清理测试时区目录失败：", err)
		code = 1
	}
	os.Exit(code)
}

func midnightFallTZData() []byte {
	// 2026 年的切换可用 32 位时间表示，按 RFC 8536 构造 v1 即可，无需 v2 数据块与页脚。
	data := make([]byte, 69)
	copy(data[:4], "TZif")
	// 版本、保留字节及 isutcnt、isstdcnt、leapcnt 均为零，不附带可选记录。
	binary.BigEndian.PutUint32(data[32:36], 1) // timecnt
	binary.BigEndian.PutUint32(data[36:40], 2) // typecnt
	binary.BigEndian.PutUint32(data[40:44], 8) // charcnt
	transition := time.Date(2026, time.November, 1, 5, 0, 0, 0, time.UTC).Unix()
	binary.BigEndian.PutUint32(data[44:48], uint32(transition))
	data[48] = 1
	// 切换只引用类型 1，使 Go 在首次切换前采用未被引用的类型 0（UTC−4 夏令时）。
	daylightOffset := int32(-4 * 60 * 60)
	standardOffset := int32(-5 * 60 * 60)
	binary.BigEndian.PutUint32(data[49:53], uint32(daylightOffset))
	data[53] = 1
	data[54] = 0
	binary.BigEndian.PutUint32(data[55:59], uint32(standardOffset))
	data[59] = 0
	data[60] = 4
	copy(data[61:], "DST\x00STD\x00")
	return data
}
