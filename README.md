# Dota 2 Replay Analyzer

打完一把不知道自己哪里打得差？这个工具可以帮你解析录像、看位置热力图，还能让 GPT 分前中后期告诉你具体做错了什么。比较简陋的初始版本，纯demo，有时间再完善。

---

## 它能做什么

**解析录像** — 把 `.dem` 文件里的玩家数据提取出来：KDA、金钱、位置轨迹、伤害等。

**热力图可视化** — 在浏览器里看每个玩家的移动轨迹，直观感受谁在哪块地图上花了多少时间。

**GPT 教练分析** — 选一个玩家，脚本会把数据喂给 GPT-4o，分前期/中期/后期三段，结合队友和对手的情况一起分析，告诉你具体哪里有问题、该怎么改。

---

## 用之前需要准备

- Go 1.21+（用来编译解析器）
- Python 3.9+，装一下 `openai`：`pip install openai`
- OpenAI API Key（GPT 分析用）

---

## 怎么用

### 第一步：编译

```bash
go build -o replay-parser.exe .
go build -o probe.exe ./probe/
```

### 第二步：解析录像

录像文件可以在 Dota 2 客户端里下载：**观察 → 最近比赛 → 下载录像**，文件在：
```
Steam/steamapps/common/dota 2 beta/game/dota/replays/<MatchID>.dem
```

然后运行：

```bash
./replay-parser.exe -o web/output.json 你的录像.dem
```

### 第三步：看热力图

```bash
cd web
python -m http.server 8080
```

浏览器打开 `http://localhost:8080`，选择玩家就能看轨迹了。

### 第四步：GPT 分析

```bash
export OPENAI_API_KEY=sk-...   # Windows 用 set 或 $env:

python analyze.py 你的录像.dem
# 会列出所有玩家让你选，也可以直接指定：
python analyze.py 你的录像.dem BroSki
python analyze.py 你的录像.dem 1
```

脚本会同时发 3 个请求，分别分析前期、中期、后期，结果用中文输出。

---

## 项目结构

```
├── main.go          # 录像解析器
├── probe/main.go    # 调试用，可以查看录像里任意实体的原始数据
├── web/             # 热力图网页（index.html + app.js + style.css）
├── analyze.py       # GPT 教练分析脚本
├── go.mod / go.sum
└── vendor/          # Go 依赖（可离线编译）
```

---

## 一些技术细节

现代 Dota 2 录像里英雄和玩家的对应关系比较绕——英雄实体的 `m_iPlayerID` 不等于玩家的 `m_nPlayerSlot`，而是玩家数据索引的 2 倍。踩了不少坑才搞清楚，本项目已经处理好了，但只支持现代格式的录像。

另外，在 Windows 上用 PowerShell 重定向输出会生成 UTF-16 文件导致网页解析报错，所以解析器用 `-o` 参数直接写文件，绕过这个问题。

---

## 依赖

- [dotabuff/manta](https://github.com/dotabuff/manta) — Dota 2 录像解析
- [openai-python](https://github.com/openai/openai-python) — GPT API

---

MIT License
