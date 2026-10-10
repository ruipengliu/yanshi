package nodesdk

import "fmt"

// ReadLimit 是读取文本文件能力返回的上限。
const ReadLimit = 64 << 10

// TruncatedNotice 是文件超过 ReadLimit 时附在返回内容之后的说明。没有它，模型会把前 64KB 当作全文，
// 据此给出错误的统计（评测 large-file-sandbox 中观察到模型先读取、后改用上传）。
func TruncatedNotice(size int64) string {
	return fmt.Sprintf("\n…【已截断：文件共 %d 字节，只返回了前 %d 字节。需要完整内容时，请用 upload_file 上传为 artifact，在云端沙箱中用代码处理。】", size, ReadLimit)
}
