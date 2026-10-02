package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/lxn/walk"
	dl "github.com/lxn/walk/declarative"
)

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
	labels := map[string]string{"occupation": "职业", "location": "城市/地区", "important_dates": "重要日子", "personality": "性格特征", "reply_length": "回复长短", "tone": "语气", "frequent_phrases": "常用表达", "emoji_usage": "表情习惯", "initiative": "主动程度", "interests": "兴趣爱好", "stressors": "压力源/雷点", "comfort_topics": "安慰话题", "when_upset": "不高兴时的表现", "closeness": "亲密程度", "recent_events": "近期共同事件", "interaction_pattern": "互动模式", "intent_patterns": "意图模式", "important_facts": "重要事实", "summary": "核心摘要"}
	var dlg *walk.Dialog
	var saveBtn, cancelBtn *walk.PushButton
	var saving bool
	var fields []dl.Widget
	var collect []func() error
	var addFields func(reflect.Value)
	addFields = func(value reflect.Value) {
		for i := 0; i < value.NumField(); i++ {
			v := value.Field(i)
			if v.Kind() == reflect.Struct {
				addFields(v)
				continue
			}
			label := labels[value.Type().Field(i).Tag.Get("json")]
			text := ""
			switch v.Kind() {
			case reflect.String:
				text = v.String()
			case reflect.Slice:
				items := []string{}
				for j := 0; j < v.Len(); j++ {
					items = append(items, v.Index(j).String())
				}
				text = strings.Join(items, "\r\n")
				label += "（每行一项）"
			case reflect.Map:
				keys := v.MapKeys()
				sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
				items := []string{}
				for _, k := range keys {
					items = append(items, k.String()+"："+v.MapIndex(k).String())
				}
				text = strings.Join(items, "\r\n")
				label += "（每行 名称：描述；清空删除）"
			}
			var editor *walk.TextEdit
			fields = append(fields, dl.Label{Text: label}, dl.TextEdit{AssignTo: &editor, Text: text, VScroll: true, MinSize: dl.Size{Width: 460, Height: 55}})
			collect = append(collect, func() error {
				s := strings.TrimSpace(editor.Text())
				switch v.Kind() {
				case reflect.String:
					v.SetString(s)
				case reflect.Slice:
					items := reflect.MakeSlice(v.Type(), 0, 0)
					for _, line := range strings.Split(s, "\n") {
						if line = strings.TrimSpace(line); line != "" {
							items = reflect.Append(items, reflect.ValueOf(line))
						}
					}
					v.Set(items)
				case reflect.Map:
					items := reflect.MakeMap(v.Type())
					for _, line := range strings.Split(s, "\n") {
						line = strings.TrimSpace(line)
						if line == "" {
							continue
						}
						pos := strings.IndexAny(line, ":：")
						if pos < 0 {
							return fmt.Errorf("意图模式请按 名称：描述 填写")
						}
						key := strings.TrimSpace(line[:pos])
						rest := strings.TrimLeft(line[pos:], ":：")
						if key == "" || items.MapIndex(reflect.ValueOf(key)).IsValid() {
							return fmt.Errorf("意图名称不能为空或重复")
						}
						items.SetMapIndex(reflect.ValueOf(key), reflect.ValueOf(strings.TrimSpace(rest)))
					}
					v.Set(items)
				}
				return nil
			})
		}
	}
	addFields(reflect.ValueOf(&profile).Elem())
	if err := (dl.Dialog{AssignTo: &dlg, Title: "编辑当前画像", Size: dl.Size{Width: 560, Height: 650}, Layout: dl.VBox{}, Children: []dl.Widget{
		dl.Label{Text: "清空字段可删除信息；再次 AI 生成可能重新提取。"},
		dl.ScrollView{Layout: dl.VBox{}, Children: fields},
		dl.Composite{Layout: dl.HBox{}, Children: []dl.Widget{
			dl.PushButton{AssignTo: &saveBtn, Text: "保存", OnClicked: func() {
				for _, get := range collect {
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
