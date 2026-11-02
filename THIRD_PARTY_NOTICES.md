# 第三方数据与许可声明

本项目（It-With-You）的**代码**与 `models/` 下的**模型 / 音色数据**是两回事。
`models/` 里的文件**全部来自第三方**，不是本项目训练或制作的；其中 `fp32/`、`int8/`
是本项目用 `prep_tts_onnx.py` 生成的**派生物**，来源与许可随原权重（见 §2）。

## 1. 数据来源

| 内容 | 来源 | 许可 | 说明 |
|---|---|---|---|
| ONNX 图与权重布局 | [High-Logic/Genie-TTS](https://github.com/High-Logic/Genie-TTS) | **MIT** | 本项目复用它的导出 / 转换结果与目录结构 |
| `t2s_*` / `vits` / `prompt_encoder` 权重 | [GPT-SoVITS](https://github.com/RVC-Boss/GPT-SoVITS)（v2ProPlus） | MIT（按其仓库声明）；**权重本身以其模型卡为准** | 由 GENIE 转换 |
| `shared/hubert/` | [TencentGameMate/chinese-hubert-base](https://huggingface.co/TencentGameMate/chinese-hubert-base) | 以模型卡为准 | 参考音频 → ssl 特征；音色准备侧的共享骨干，一份即可，不随音色增长 |
| `shared/speaker/speaker_encoder.onnx` | eres2netv2（ModelScope） | 以模型卡为准 | 说话人向量 |
| `shared/roberta/RoBERTa.onnx` + `tokenizer/` | chinese-roberta-wwm-ext-large | 以模型卡为准 | 中文韵律，**可选**；整目录可删，不影响链路跑通 |
| `shared/g2p/`（`symbols_v2.json`、`ChineseG2P/opencpop-strict.txt`） | 同 GENIE | **MIT** | 音素表与词典，纯数据 |
| `voices/feibi/`（参考音频 + 权重） | GENIE 的预置音色 | **不属于本项目** | 菲比（《鸣潮》）；**仅用于本地可行性验证，禁止再分发** |

> **一句话**：GENIE 是 MIT，但 MIT 只覆盖**它的代码**，不覆盖上游模型权重，更不覆盖它的**预置游戏角色音色**。

### 1.1 目录结构（建议布局，文件尚未移动）

按"共享 / 音色"与"原始 / 中间 / 交付"两个维度分层。分层只影响存放位置，不改变来源与许可。

```
<repo>/
├── tools/
│   └── prep_tts_onnx.py            ← 本项目代码，应纳入版本库（现在在 models/ 下被 gitignore 了）
└── models/                         ← .gitignore：只放第三方数据与派生物
    ├── shared/                     ← 共享骨干：一份，不随音色增长
    │   ├── hubert/
    │   │   ├── raw/                chinese-hubert-base.onnx + *_weights_fp16.bin
    │   │   ├── fp32/               中间产物（可重建，可删）
    │   │   └── int8/               ← 音色准备侧实际使用
    │   ├── speaker/
    │   │   └── raw/                speaker_encoder.onnx
    │   ├── roberta/                （可选，整目录可删）
    │   │   ├── raw/                RoBERTa.onnx
    │   │   └── tokenizer/
    │   └── g2p/                    symbols_v2.json + ChineseG2P/
    └── voices/
        └── feibi/                  ← 仅本机验证，禁止再分发
            ├── ref/                prompt_wav/*.wav + prompt_wav.json（音色的本质）
            └── tts/
                ├── raw/            GENIE 原样：5 个 .onnx + 3 个 *_fp16.bin
                ├── fp32/           中间产物（可重建，可删）
                └── int8/           ← 运行时只读这里：4 个 int8 + vits 仍 fp32
```

现状 → 目标（手动搬迁用）：

| 现在 | 目标 |
|---|---|
| `models/GenieData/chinese-hubert-base/` | `models/shared/hubert/`（把原 `.onnx` 收进 `raw/`，原 `fp32/`、`int8/` 提到同级） |
| `models/GenieData/speaker_encoder.onnx` | `models/shared/speaker/raw/` |
| `models/GenieData/RoBERTa/{RoBERTa.onnx,roberta_tokenizer/}` | `models/shared/roberta/{raw/,tokenizer/}` |
| `models/GenieData/G2P/` | `models/shared/g2p/`（`__pycache__` 不搬，直接删） |
| `models/CharacterModels/v2ProPlus/feibi/prompt_wav*` | `models/voices/feibi/ref/` |
| `…/feibi/tts_models/*.onnx` 与 `*_fp16.bin` | `models/voices/feibi/tts/raw/` |
| `…/feibi/tts_models/fp32/`、`int8/` | `models/voices/feibi/tts/` 下同名子目录 |
| `models/_tools/prep_tts_onnx.py` | `tools/prep_tts_onnx.py` |

⚠️ 搬迁后 `prep_tts_onnx.py` 里的 `TTS_DIR` / `HUBERT_DIR` 常量必须同步改，否则脚本会找不到模型。
⚠️ 这里的 `models/` 是**开发期布局**；运行时随包分发的 `tts/` 布局见 `TTS原生go方案.md` §5，两者不是一回事。

## 2. 分发政策

- `models/` 下的 `.onnx` / `.bin` / `.data` / `.wav` / 音色配置**不进入版本库**（见 `.gitignore`）。
- 发布安装包时**不得**包含 `voices/` 下的任何预置音色（当前只有 `feibi`，仅限本机验证）；必须替换为**自制或已获明确授权**的音色。
- **`fp32/` 与 `int8/` 是同一份上游权重的变换产物**：量化、格式转换、外置数据重打包都**不产生新的权利**。把 int8 随包分发 = 分发同一份权重，许可判定与原始权重完全一致。
- 即使替换了音色，模型权重的上游许可仍需逐项核对（hubert / roberta / eres2net / gpt-sovits）。
- 如果后续把模型随包分发，安装包里应带一份本文件（或它的摘要）。

## 3. 本项目自己的代码

仓库目前**没有 `LICENSE` 文件**。在补上之前，"开源"只是口头描述，不构成法律意义上的授权。
建议尽快选定一个（例如 MIT / Apache-2.0），并明确它**只覆盖本项目代码**，不覆盖 `models/` 与
`TTS原生go方案.md` 中引用的第三方内容。

## 4. 如何核对

- GENIE 的许可原文：仓库根目录 `LICENSE`（MIT，`Copyright (c) 2025 High_Logic`）
- 上游模型：打开对应模型卡，确认 `license` 字段
- 音色：确认参考音频的来源与再分发许可；**没有明确许可的一律按"不可分发"处理**
