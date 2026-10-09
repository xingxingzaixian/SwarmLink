package message

import (
	"encoding/json"
	"testing"
)

// 文件夹消息的 content 必须是可解析的 JSON 信封，且字段是 snake_case。
//
// 这条是踩过的坑：FileMessageContent 曾经没有 tag，序列化出 JobID/FileName，
// 前端一个字段都读不到，只能显示成一段裸 JSON。
func TestFolderMessageContentJSONTags(t *testing.T) {
	c := FolderMessageContent{
		BatchID: "b1", FolderName: "照片", FileCount: 32, TotalSize: 1024, Skipped: 2,
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"batch_id", "folder_name", "file_count", "total_size", "skipped"} {
		if _, ok := m[k]; !ok {
			t.Errorf("缺少 snake_case 字段 %q（前端按它解析）: %s", k, b)
		}
	}

	var back FolderMessageContent
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back != c {
		t.Errorf("往返不一致：%+v ≠ %+v", back, c)
	}
}

// 消息类型取值不能与既有类型撞车（撞车的表现是「文件卡片渲染成了文件夹」）。
func TestMsgTypeFolderDistinct(t *testing.T) {
	for _, other := range []MsgType{MsgTypeText, MsgTypeImage, MsgTypeFile, MsgTypeFileFailed} {
		if MsgTypeFolder == other {
			t.Fatalf("MsgTypeFolder 与 %q 撞车", other)
		}
	}
}
