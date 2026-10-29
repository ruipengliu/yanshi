package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"yanshi/internal/ids"
)

// CallMemoryID 的命名空间包括真实归属与完整调用身份；同槽不同用户不能共享去重对象。
// 长度前缀保留原始字节与字段边界，SHA-256 使斜杠、Unicode 等外部值也成为合法 token。
func CallMemoryID(r SaveRequest) (string, error) {
	h := sha256.New()
	for _, value := range []string{r.EndUser, r.BusinessLine, r.SessionID, r.CallID} {
		h.Write([]byte(strconv.Itoa(len(value)) + ":" + value))
	}
	// 取前 128 位，与随机 ID 的强度相同；模型会在召回与 memory_forget 中读写这个 ID，不宜过长。
	return ids.WithEndUser("mem", hex.EncodeToString(h.Sum(nil)[:16]), r.EndUser)
}
