package feishu

import (
	"reflect"
	"testing"

	livechat "github.com/KevinGong2013/feishu-livechat"
)

func text(s string) livechat.Part  { return livechat.Part{Text: s} }
func image(k string) livechat.Part { return livechat.Part{ImageKey: k} }

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		msgType string
		content string
		keys    []string
		want    []livechat.Part
	}{
		{
			name:    "text with bot mention",
			msgType: "text",
			content: `{"text":"@_user_1 您好，问题已经修复了"}`,
			keys:    []string{"@_user_1"},
			want:    []livechat.Part{text("您好，问题已经修复了")},
		},
		{
			name:    "text with several mentions, longest key first",
			msgType: "text",
			content: `{"text":"@_user_12 你看下 @_user_1 好的 @_all"}`,
			keys:    []string{"@_user_1", "@_user_12"},
			want:    []livechat.Part{text("你看下 好的")},
		},
		{
			name:    "text that is only a mention",
			msgType: "text",
			content: `{"text":"@_user_1 "}`,
			keys:    []string{"@_user_1"},
			want:    nil,
		},
		{
			name:    "post as delivered in events (unwrapped)",
			msgType: "post",
			content: `{"title":"","content":[[{"tag":"at","user_id":"@_user_1","user_name":"客服机器人"},{"tag":"text","text":" 请按截图操作："}],[{"tag":"img","image_key":"img_v3_a","width":800,"height":600}],[{"tag":"text","text":"然后点保存"}]]}`,
			want:    []livechat.Part{text("请按截图操作："), image("img_v3_a"), text("然后点保存")},
		},
		{
			name:    "post wrapped in a locale, with title and link",
			msgType: "post",
			content: `{"zh_cn":{"title":"排查结果","content":[[{"tag":"text","text":"详见 "},{"tag":"a","text":"文档","href":"https://example.com/doc"}],[{"tag":"a","text":"https://example.com","href":"https://example.com"}]]}}`,
			want:    []livechat.Part{text("排查结果\n详见 文档 (https://example.com/doc)\nhttps://example.com")},
		},
		{
			name:    "post with images back to back and unsupported elements",
			msgType: "post",
			content: `{"content":[[{"tag":"img","image_key":"k1"},{"tag":"img","image_key":"k2"}],[{"tag":"emotion","emoji_type":"SMILE"},{"tag":"media","file_key":"f","image_key":"cover"},{"tag":"hr"}]]}`,
			want:    []livechat.Part{image("k1"), image("k2")},
		},
		{
			name:    "post mention in the middle of a line",
			msgType: "post",
			content: `{"content":[[{"tag":"text","text":"麻烦 "},{"tag":"at","user_id":"@_user_2","user_name":"小王"},{"tag":"text","text":" 跟进"}]]}`,
			want:    []livechat.Part{text("麻烦 跟进")},
		},
		{
			name:    "post with only content_v2 markdown",
			msgType: "post",
			content: `{"zh_cn":{"content_v2":[[{"tag":"md","text":"<at user_id=\"ou_bot\">机器人</at> 看这里 ![image](img_md) 就好"}]]}}`,
			want:    []livechat.Part{text("看这里"), image("img_md"), text("就好")},
		},
		{
			name:    "post prefers structured content over content_v2",
			msgType: "post",
			content: `{"zh_cn":{"content":[[{"tag":"text","text":"structured"}]],"content_v2":[[{"tag":"md","text":"markdown"}]]}}`,
			want:    []livechat.Part{text("structured")},
		},
		{
			name:    "image message (cannot carry a mention) yields nothing",
			msgType: "image",
			content: `{"image_key":"img"}`,
			want:    nil,
		},
		{
			name:    "malformed content",
			msgType: "post",
			content: `not json`,
			want:    nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Parse(tt.msgType, tt.content, tt.keys)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Parse() =\n%#v\nwant\n%#v", got, tt.want)
			}
		})
	}
}
