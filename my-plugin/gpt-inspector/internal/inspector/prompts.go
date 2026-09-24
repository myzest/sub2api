package inspector

type Prompt struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Kind  string `json:"kind"`
	Text  string `json:"text"`
}

var prompts = []Prompt{
	{"pelican", "鹈鹕骑自行车", "html", "请生成可直接运行的单文件HTML，使用内联SVG绘制鹈鹕骑自行车的二维循环动画。画面以鹈鹕和自行车为主体，展示清晰的身体结构、踩踏动作和车轮转动，配合协调的背景、配色与层次。动画应流畅自然、衔接连续，并适配不同屏幕尺寸。禁止依赖外部资源，只输出完整HTML，不要代码围栏或解释文字。"},
	{"candy", "摸糖问题", "text", "袋子里有三种口味的糖——苹果、桃子、西瓜。每种口味又有两种形状，圆形和五角星。摸糖的时候，形状靠手感就能分辨。\n\n不同口味和形状的数量如下。\n\n          苹果 桃子 西瓜\n圆形       7    9    8\n五角星     7    6    4\n\n问题是：最少取多少颗，能保证手里同时有一组「圆形苹果 + 五角星桃子」或者「五角星苹果 + 圆形桃子」？"},
	{"iphone", "最新苹果手机", "text", "你的知识库截止到什么时候?不联网直接告诉我你知道的最新的苹果手机型号是什么"},
	{"japan_pm", "日本首相", "text", "你的知识训练截至到什么时候？你所知道的日本首相目前是谁，不要联网搜索，直接回答我"},
}

func findPrompt(id string) (Prompt, bool) {
	for _, p := range prompts {
		if p.ID == id {
			return p, true
		}
	}
	return Prompt{}, false
}
