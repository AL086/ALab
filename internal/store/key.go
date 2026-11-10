package store

// ObjectKey 是内容寻址的存储 key：objects/ab/cd/<sha256>。
// 两段前缀是为了避免单目录下堆几十万文件。
//
// 这是全项目唯一的定义，api 与后台清理任务都调它，不要各写一份。
func ObjectKey(sha string) string {
	if len(sha) < 4 {
		return "objects/" + sha
	}
	return "objects/" + sha[0:2] + "/" + sha[2:4] + "/" + sha
}
