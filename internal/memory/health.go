package memory

import "strings"

// healthTerms 是认定一条 Memory 属于健康信息的用词（ADR-0022，docs/design/m4-memory-grant.md §2）。类别由模型自报，
// 健康信息写成 relationship、preference 就绕过了业务线的同意；内容中出现这些词时只能写入 health 类别。
//
// 只收在"关于人的一句陈述"中几乎只指身心健康的词，宁可漏掉也不误伤：误判会让正常的记忆被拒绝或被归进 health
// 而不再召回。不收"医生"（"张三是医生"是职业）、"感冒"（"对加班不感冒"）、"健康"（"喜欢健康饮食"是偏好）、
// "压力"（"压力测试"）、"诊断"（"故障诊断"）、"复查"（"复查合同"）、"药物""手术""体检"（"药物研发""手术室护士"
// "体检中心"）与单字的"病""药""疼"；这些只收表示就医、用药状态的说法。这是兜底，不是分类器：换个说法的健康信息
// 仍要靠写入指令；只认中文用词。
var healthTerms = []string{
	// 过敏、慢性病与指标
	"过敏", "乳糖不耐受", "血压", "收缩压", "舒张压", "血糖", "血脂", "胆固醇", "糖尿病", "心脏病", "哮喘", "癫痫",
	"癌症", "慢性病", "病史", "胃病", "胃炎", "胃溃疡", "胃不好", "透析",
	// 用药与就医
	"用药", "服药", "服用", "吃药", "降压药", "胰岛素", "安眠药", "住院", "做手术", "动手术", "做体检", "确诊", "就医",
	"医嘱", "看医生", "看病",
	// 睡眠与心理
	"失眠", "睡不着", "睡不好", "抑郁", "焦虑", "心理咨询", "心理治疗", "看心理医生",
	// 生育
	"怀孕", "孕期", "备孕", "哺乳",
	"残疾",
}

// healthExcepts 是含有健康用词、却说的是职业的说法：匹配之前先去掉。
var healthExcepts = []string{"住院医师", "心理咨询师"}

// healthTerm 返回 content 中第一个健康信息用词，没有时为空。
func healthTerm(content string) string {
	for _, e := range healthExcepts {
		content = strings.ReplaceAll(content, e, "")
	}
	for _, w := range healthTerms {
		if strings.Contains(content, w) {
			return w
		}
	}
	return ""
}
