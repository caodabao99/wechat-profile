package main

import "testing"

func TestParseClipboardChineseDate(t *testing.T) {
	text := `小明 2025年6月10日 10:23:45
你好啊
在吗
小明 2025年6月10日 10:24:01
周末有空吗
我 2025年6月10日 10:25:00
有啊，怎么了`
	msgs := ParseClipboard(text, "我")
	if len(msgs) != 3 {
		t.Fatalf("期望 3 条消息，实际 %d: %+v", len(msgs), msgs)
	}
	if msgs[0].SenderName != "小明" || msgs[0].Sender != "other" {
		t.Fatalf("第1条解析错误: %+v", msgs[0])
	}
	if msgs[0].Content != "你好啊\n在吗" {
		t.Fatalf("第1条多行内容错误: %q", msgs[0].Content)
	}
	if msgs[2].Sender != "me" {
		t.Fatalf("第3条应为 me: %+v", msgs[2])
	}
	if name := InferContactName(msgs, "我"); name != "小明" {
		t.Fatalf("联系人为 小明，实际 %q", name)
	}
}

func TestParseClipboardSlashDate(t *testing.T) {
	text := `小红 2025/6/10 10:23:45
哈喽`
	msgs := ParseClipboard(text, "我")
	if len(msgs) != 1 || msgs[0].SenderName != "小红" {
		t.Fatalf("斜杠日期解析错误: %+v", msgs)
	}
}

func TestParseClipboardDashDate(t *testing.T) {
	text := `小红 2025-6-10 10:23
哈喽`
	msgs := ParseClipboard(text, "我")
	if len(msgs) != 1 || msgs[0].SenderName != "小红" {
		t.Fatalf("横杠日期解析错误: %+v", msgs)
	}
}

func TestParseClipboardSplitLines(t *testing.T) {
	// 新版微信：昵称、时间各占一行
	text := `小刚
2025年6月10日 10:23:45
今晚一起吃饭吗
我
2025年6月10日 10:24:00
好的`
	msgs := ParseClipboard(text, "我")
	if len(msgs) != 2 {
		t.Fatalf("分行格式期望 2 条，实际 %d: %+v", len(msgs), msgs)
	}
	if msgs[0].SenderName != "小刚" || msgs[0].Content != "今晚一起吃饭吗" {
		t.Fatalf("分行第1条错误: %+v", msgs[0])
	}
	if msgs[1].Sender != "me" {
		t.Fatalf("分行第2条应为 me: %+v", msgs[1])
	}
}

func TestParseClipboardColonFallback(t *testing.T) {
	text := "老王: 明天见"
	msgs := ParseClipboard(text, "我")
	if len(msgs) != 1 || msgs[0].SenderName != "老王" || msgs[0].Content != "明天见" {
		t.Fatalf("冒号降级解析错误: %+v", msgs)
	}
}

func TestParseClipboardPlainTextFallback(t *testing.T) {
	text := "随便一段没有格式的文本"
	msgs := ParseClipboard(text, "我")
	if len(msgs) != 1 || msgs[0].Sender != "other" || msgs[0].Content != text {
		t.Fatalf("纯文本降级错误: %+v", msgs)
	}
}
