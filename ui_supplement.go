package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/lxn/walk"
	dl "github.com/lxn/walk/declarative"
)

// showEditProfileDialog 编辑画像。分节顺序、分节名、字段标签必须与
// profileFullText（画像查看页）逐字一致，避免编辑时面对另一套自造字段名。
func showEditProfileDialog(id int64, onDone func()) {
	contact, err := fetchContactByID(id)
	if err != nil {
		showError(err.Error())
		return
	}
	var profile Profile
	if strings.TrimSpace(contact.ProfileJSON) != "" {
		if err := json.Unmarshal([]byte(contact.ProfileJSON), &profile); err != nil {
			showError("画像无法解析: " + err.Error())
			return
		}
	}
	var dlg *walk.Dialog
	var saveBtn, cancelBtn *walk.PushButton
	var sv *walk.ScrollView
	var saving bool
	var widgets []dl.Widget
	var collectors []func() error

	// 分节标题与画像查看页的【节名】相同
	section := func(title string) {
		widgets = append(widgets,
			dl.Label{Text: "【" + title + "】", Font: fontSection},
		)
	}
	hint := func(text string) {
		widgets = append(widgets, dl.Label{Text: text, Font: fontHint})
	}
	// 单值文本字段：label 为空表示整节就是一个段落（概要）
	strField := func(label string, val string, target *string, height int) {
		if label != "" {
			widgets = append(widgets, dl.Label{Text: label, Font: fontBody})
		}
		var te *walk.TextEdit
		widgets = append(widgets, dl.TextEdit{
			AssignTo: &te, Text: val, VScroll: true, Font: fontBody,
			// 固定高度 + 内部滚动：高度只由 MinSize 决定，文字不会被拦腰截断
			MinSize: dl.Size{Height: height},
		})
		collectors = append(collectors, func() error {
			*target = strings.TrimSpace(te.Text())
			return nil
		})
	}
	// 列表字段：每行一项；setter 单独传入，兼容 ImportantDates 这类自定义切片类型
	listField := func(label, note string, vals []string, set func([]string), height int) {
		if label != "" {
			text := label
			if note != "" {
				text += "（" + note + "）"
			}
			widgets = append(widgets, dl.Label{Text: text, Font: fontBody})
		} else if note != "" {
			hint(note)
		}
		var te *walk.TextEdit
		widgets = append(widgets, dl.TextEdit{
			AssignTo: &te, Text: strings.Join(vals, "\r\n"), VScroll: true, Font: fontBody,
			MinSize: dl.Size{Height: height},
		})
		collectors = append(collectors, func() error {
			items := []string{}
			for _, line := range strings.Split(te.Text(), "\n") {
				if line = strings.TrimSpace(line); line != "" {
					items = append(items, line)
				}
			}
			set(items)
			return nil
		})
	}

	// 1. 概要
	section("概要")
	strField("", profile.Summary, &profile.Summary, 50)
	// 2. 基本信息
	section("基本信息")
	strField("职业", profile.BasicInfo.Occupation, &profile.BasicInfo.Occupation, 30)
	strField("所在地", profile.BasicInfo.Location, &profile.BasicInfo.Location, 30)
	listField("重要日子", "每行一项", []string(profile.BasicInfo.ImportantDates),
		func(x []string) { profile.BasicInfo.ImportantDates = ImportantDates(x) }, 60)
	// 3. 性格特征
	section("性格特征")
	listField("", "每行一项", profile.Personality, func(x []string) { profile.Personality = x }, 70)
	// 4. 沟通风格
	section("沟通风格")
	strField("回复长短", profile.CommunicationStyle.ReplyLength, &profile.CommunicationStyle.ReplyLength, 30)
	strField("语气", profile.CommunicationStyle.Tone, &profile.CommunicationStyle.Tone, 30)
	listField("口头禅", "每行一项", profile.CommunicationStyle.FrequentPhrases,
		func(x []string) { profile.CommunicationStyle.FrequentPhrases = x }, 60)
	strField("表情使用", profile.CommunicationStyle.EmojiUsage, &profile.CommunicationStyle.EmojiUsage, 30)
	strField("主动程度", profile.CommunicationStyle.Initiative, &profile.CommunicationStyle.Initiative, 30)
	// 5. 兴趣爱好
	section("兴趣爱好")
	listField("", "每行一项", profile.Interests, func(x []string) { profile.Interests = x }, 70)
	// 6. 情绪模式
	section("情绪模式")
	listField("压力源", "每行一项", profile.EmotionalPatterns.Stressors,
		func(x []string) { profile.EmotionalPatterns.Stressors = x }, 60)
	listField("安慰有效话题", "每行一项", profile.EmotionalPatterns.ComfortTopics,
		func(x []string) { profile.EmotionalPatterns.ComfortTopics = x }, 60)
	strField("不高兴时的表现", profile.EmotionalPatterns.WhenUpset, &profile.EmotionalPatterns.WhenUpset, 45)
	// 7. 关系
	section("关系")
	strField("亲密程度", profile.Relationship.Closeness, &profile.Relationship.Closeness, 30)
	listField("近期共同事件", "每行一项", profile.Relationship.RecentEvents,
		func(x []string) { profile.Relationship.RecentEvents = x }, 60)
	strField("互动模式", profile.Relationship.InteractionPattern, &profile.Relationship.InteractionPattern, 45)
	// 8. 典型意图（map：每行 名称：描述）
	section("典型意图")
	intentKeys := make([]string, 0, len(profile.IntentPatterns))
	for k := range profile.IntentPatterns {
		intentKeys = append(intentKeys, k)
	}
	sort.Strings(intentKeys)
	intentLines := make([]string, 0, len(intentKeys))
	for _, k := range intentKeys {
		intentLines = append(intentLines, k+"："+profile.IntentPatterns[k])
	}
	hint("每行一条：名称：描述；整行清空即删除")
	var intentTE *walk.TextEdit
	widgets = append(widgets, dl.TextEdit{
		AssignTo: &intentTE, Text: strings.Join(intentLines, "\r\n"), VScroll: true, Font: fontBody,
		MinSize: dl.Size{Height: 90},
	})
	collectors = append(collectors, func() error {
		items := map[string]string{}
		for _, line := range strings.Split(intentTE.Text(), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			pos := strings.IndexAny(line, ":：")
			if pos < 0 {
				return fmt.Errorf("典型意图请按 名称：描述 填写")
			}
			key := strings.TrimSpace(line[:pos])
			val := strings.TrimSpace(strings.TrimLeft(line[pos:], ":："))
			if key == "" {
				return fmt.Errorf("意图名称不能为空")
			}
			if _, dup := items[key]; dup {
				return fmt.Errorf("意图名称不能重复：" + key)
			}
			items[key] = val
		}
		profile.IntentPatterns = items
		return nil
	})
	// 9. 重要事实
	section("重要事实")
	listField("", "每行一项", profile.ImportantFacts, func(x []string) { profile.ImportantFacts = x }, 70)

	if err := (dl.Dialog{AssignTo: &dlg, Title: "编辑当前画像",
		// walk 的 Dialog.Show 按 max(布局最小尺寸, MinSize) 开窗，开屏即 720×820，
		// 超出工作区时由 clampDialogOpenSize 钳制，放不下的内容由 ScrollView 滚动
		MinSize: dl.Size{Width: 720, Height: 820}, Size: dl.Size{Width: 720, Height: 820},
		Layout: dl.VBox{Spacing: 6}, Children: []dl.Widget{
			dl.Label{Text: "分节与画像页一致；清空字段或列表项即可删除，再次 AI 生成可能重新提取。", Font: fontHint},
			// 关掉横向滚动：字段宽度全部自适应窗口，不再撑出横向滚动条
			dl.ScrollView{AssignTo: &sv, HorizontalFixed: true, Layout: dl.VBox{Spacing: 4}, Children: widgets},
			dl.Composite{Layout: dl.HBox{}, Children: []dl.Widget{
				dl.PushButton{AssignTo: &saveBtn, Text: "保存", OnClicked: func() {
					for _, get := range collectors {
						if err := get(); err != nil {
							showError(err.Error())
							return
						}
					}
					saving = true
					saveBtn.SetEnabled(false)
					cancelBtn.SetEnabled(false)
					go func(p Profile) {
						err := doEditProfile(id, p, contact.ProfileJSON)
						mainWindow.Synchronize(func() {
							if dlg.IsDisposed() {
								return
							}
							saving = false
							saveBtn.SetEnabled(true)
							cancelBtn.SetEnabled(true)
							if err != nil {
								showError(err.Error())
								return
							}
							dlg.Accept()
							if onDone != nil {
								onDone()
							}
						})
					}(profile)
				}},
				dl.PushButton{AssignTo: &cancelBtn, Text: "取消", OnClicked: func() { dlg.Cancel() }},
			}},
		}}).Create(mainWindow); err != nil {
		showError(err.Error())
		return
	}
	setTopMost(dlg.Handle())
	// 可拖拽缩放/最大化 + 开屏尺寸钳制到工作区；位置不干预
	// （walk 默认以悬浮窗为中心定位，与画像窗等其他弹窗一致靠右显示）
	makeDialogResizable(dlg)
	clampDialogOpenSize(dlg)
	dlg.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		if saving {
			*canceled = true
		}
	})
	dlg.Run()
}

// showSupplementDialog 弹出手动补充画像信息的对话框
func showSupplementDialog(contact Contact, onDone func()) {
	var dlg *walk.Dialog
	var noteTE *walk.TextEdit
	// 不叫 statusLabel：那是主窗口状态栏的包级变量，遮蔽之后很容易改错地方
	var hintLabel *walk.Label
	var submitBtn, cancelBtn *walk.PushButton

	if err := (dl.Dialog{
		AssignTo: &dlg,
		Title:    "补充画像信息",
		MinSize:  dl.Size{Width: 420, Height: 320},
		Layout:   dl.VBox{Spacing: 6},
		Children: []dl.Widget{
			dl.Label{
				Text: fmt.Sprintf("给「%s」手动补充信息", displayName(&contact)),
				Font: fontSection,
			},
			dl.Label{
				Text: "比如：生日 5月20日、性别男、星座金牛、手机号 138xxx、职业设计师…",
				Font: fontHint,
			},
			dl.TextEdit{
				AssignTo: &noteTE,
				VScroll:  true,
				Font:     fontBody,
				MinSize:  dl.Size{Width: 380, Height: 140},
			},
			dl.Label{AssignTo: &hintLabel, Text: "", Font: fontHint},
			dl.Composite{
				Layout: dl.HBox{MarginsZero: true},
				Children: []dl.Widget{
					dl.HSpacer{},
					dl.PushButton{
						AssignTo: &submitBtn,
						Text:     "提交",
						MinSize:  dl.Size{Width: 80, Height: 32},
						OnClicked: func() {
							note := strings.TrimSpace(noteTE.Text())
							if note == "" {
								showError("请先输入要补充的信息")
								return
							}
							// 一次补充要跑一轮 LLM（最长约两分钟），期间必须锁住按钮：
							// 否则连点几下就会并发改同一条画像，历史记录里全是重复版本。
							submitBtn.SetEnabled(false)
							cancelBtn.SetEnabled(false)
							hintLabel.SetText("正在合并到画像…")
							go func() {
								err := doSupplement(contact.ID, note)
								mainWindow.Synchronize(func() {
									// 请求还在飞的时候用户可能已经关掉了对话框，
									// 再操作已销毁的控件会崩
									if dlg.IsDisposed() {
										return
									}
									if err != nil {
										submitBtn.SetEnabled(true)
										cancelBtn.SetEnabled(true)
										hintLabel.SetText("")
										showError("补充画像失败: " + err.Error())
										return
									}
									dlg.Accept()
									if onDone != nil {
										onDone()
									}
								})
							}()
						},
					},
					dl.PushButton{
						AssignTo:  &cancelBtn,
						Text:      "取消",
						MinSize:   dl.Size{Width: 60, Height: 32},
						OnClicked: func() { dlg.Cancel() },
					},
				},
			},
		},
	}).Create(mainWindow); err != nil {
		showError("创建补充画像对话框失败: " + err.Error())
		return
	}
	setTopMost(dlg.Handle())
	dlg.Run()
}
