package gateway

import (
	"context"
	"log"
	"os"
)

// bootstrapFromEnv 在全新数据库（没有任何渠道）上，根据环境变量中的上游凭证创建初始渠道。
// 凭证只在服务端进程内使用，不写日志、不下发浏览器。价格为占位参考价，需按上游账单核对。
func (s *Server) bootstrapFromEnv() {
	var n int
	if s.db.QueryRow(`SELECT COUNT(*) FROM channels`).Scan(&n) != nil || n > 0 {
		return
	}
	type price struct {
		kind, mode       string
		in, out, unit    int64
		maxOut, defaultM int64
	}
	const note = "占位参考价，请按上游实际账单核对后修改"
	setPrice := func(name string, p price) {
		s.db.Exec(`INSERT INTO models(name, kind, billing_mode, price_in, price_out, price_unit, priced, max_output_tokens, default_max_tokens, note, updated_at)
			VALUES(?,?,?,?,?,?,1,?,?,?,?) ON CONFLICT(name) DO UPDATE SET kind=excluded.kind, billing_mode=excluded.billing_mode, price_in=excluded.price_in,
			price_out=excluded.price_out, price_unit=excluded.price_unit, priced=1, max_output_tokens=excluded.max_output_tokens, default_max_tokens=excluded.default_max_tokens, note=excluded.note`,
			name, p.kind, p.mode, p.in, p.out, p.unit, p.maxOut, p.defaultM, note, nowMs())
	}
	yuanM := func(y float64) int64 { return int64(y * 1e6) }
	add := func(name, typ, key string, models []string, fetch bool) {
		if key == "" {
			return
		}
		if fetch {
			if list, err := s.fetchUpstreamModels(context.Background(), typ, channelTypes[typ], key); err == nil {
				models = list
			} else {
				log.Printf("初始化渠道 %s：获取上游模型列表失败，使用已登记的模型名（%v）", name, err)
			}
		}
		c := &Channel{Name: name, Type: typ, BaseURL: channelTypes[typ], APIKey: key, Status: 1, Weight: 1}
		for _, m := range models {
			c.Models = append(c.Models, ChannelModel{Model: m, UpstreamModel: m})
		}
		if err := s.saveChannel(c); err != nil {
			log.Printf("初始化渠道 %s 失败: %v", name, err)
			return
		}
		log.Printf("已根据环境变量初始化渠道：%s（%d 个模型）", name, len(models))
		for _, m := range models {
			switch guessKind(m) {
			case "chat":
				setPrice(m, price{kind: "chat", mode: "token", in: yuanM(2), out: yuanM(8), defaultM: 4096})
			case "embedding":
				setPrice(m, price{kind: "embedding", mode: "token", in: yuanM(0.5)})
			case "tts":
				setPrice(m, price{kind: "tts", mode: "char", in: yuanM(350)}) // 3.5 元 / 万字符
			case "music":
				setPrice(m, price{kind: "music", mode: "request", unit: yuanM(1)})
			case "video":
				if typ == "volcengine" {
					setPrice(m, price{kind: "video", mode: "token", out: yuanM(46)})
				} else {
					setPrice(m, price{kind: "video", mode: "request", unit: yuanM(3)})
				}
			}
		}
	}
	add("DeepSeek", "deepseek", os.Getenv("DEEPSEEK_API_KEY"), []string{"deepseek-v4-flash", "deepseek-v4-pro"}, true)
	add("智谱 BigModel", "zhipu", os.Getenv("BIGMODEL_API_KEY"), []string{"embedding-3"}, false)
	add("MiniMax", "minimax", os.Getenv("MINIMAX_API_KEY"), []string{"MiniMax-H3", "speech-2.8-hd", "speech-2.8-turbo", "music-3.0", "music-2.6"}, false)
	add("火山方舟 Seedance", "volcengine", os.Getenv("ARK_API_KEY"), []string{"doubao-seedance-2-0-260128", "doubao-seedance-2-0-fast-260128"}, false)
	if os.Getenv("MINIMAX_API_KEY") != "" {
		s.db.Exec(`UPDATE models SET price_unit=?, priced=1, note=? WHERE name IN ('voice-clone','voice-design')`, yuanM(9.9), note)
	}
}
