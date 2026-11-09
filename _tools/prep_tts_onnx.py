# -*- coding: utf-8 -*-
"""GPT-SoVITS / GENIE 权重的一次性准备工具（**不进运行时**，Go 侧只读产物）。

为什么需要它
------------
GENIE 导出的 ONNX 是「**fp32 的图 + fp16 的权重**」：
  - `.onnx` 里 initializer 的 `data_type` 是 FLOAT（1），形状也按 fp32 写；
  - 真正的权重存在外部的 `*_fp16.bin` 里，字节数只有 fp32 的一半。

严格的 ONNX Runtime 加载器会按 `data_type` 去校验外部数据的字节数，
于是直接 load 会报 external data 尺寸不符 —— 这不是"文件缺了"，
而是 GENIE 在自己的加载器里**把 fp16 上采样成 fp32** 之后再喂给 ORT。
本工具就是把这一步（以及可选的 int8 量化）做一次、落盘，让 Go 侧能直接加载。

子命令
------
  inspect   只读：列出每个 initializer 的期望字节数与外部文件位置，确认"哪些是 fp16 存的"
  fp32      上采样：生成自包含的 fp32 ONNX（量化与验证的中间产物）
  int8      动态量化：fp32 → int8（weight-only，MatMul/Gemm）
  verify    用假输入跑一遍 ORT，确认能加载、能出结果

用法示例
--------
  python _tools/prep_tts_onnx.py inspect
  python _tools/prep_tts_onnx.py fp32   --only t2s_encoder
  python _tools/prep_tts_onnx.py fp32
  python _tools/prep_tts_onnx.py int8   --only t2s_encoder
  python _tools/prep_tts_onnx.py verify

⚠️ 本工具在 `_tools/`，纳入版本库；只有 `models/`（第三方数据与产物）被 .gitignore 忽略。
"""

import argparse
import glob
import os
import sys
import time

import numpy as np
import onnx
from onnx import numpy_helper

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
MODELS = os.path.join(ROOT, "models")

# 需要处理的两组模型
#   1) 单个角色的 TTS 四件套 + vits（models/voices/<音色>/tts）
#   2) 音色准备用的 chinese-hubert-base（models/shared/hubert，每角色一次预计算，运行时不加载）
TTS_DIR = os.path.join(MODELS, "voices")
HUBERT_DIR = os.path.join(MODELS, "shared", "hubert")


# ---------- 基础 ----------

def _ext_map(init):
    """把 external_data 的 repeated 结构转成 dict。"""
    return {e.key: e.value for e in init.external_data}


def _elems(init):
    n = 1
    for d in init.dims:
        n *= d
    return n


def _resolve(base_dir, location):
    """定位外部权重文件。

    GENIE 导出的 ONNX 里，外部文件名与磁盘上的名字对不上，两种常见情形：
      - `prompt_encoder_fp32.bin` / `t2s_shared_fp32.bin` / `vits_fp32.bin`
        → 磁盘上是 `*_fp16.bin`（**图声明 fp32、数据存 fp16**）
      - hubert 写成 `..._weights.bin` → 磁盘上是 `..._weights_fp16.bin`
    所以按顺序兜底，返回 (路径, 是否走了兜底)。
    """
    for cand, fb in [
        (location, False),
        (location.replace("_fp32.bin", "_fp16.bin"), True),
        (os.path.splitext(location)[0] + "_fp16.bin", True),
    ]:
        p = os.path.join(base_dir, cand)
        if os.path.exists(p):
            return p, fb
    return os.path.join(base_dir, location), False


def _file_scales(model, base_dir):
    """判断每个外部权重文件里**实际**存的是 fp16 还是 fp32，返回 {路径: 0.5 或 1.0}。

    为什么必须实测、不能信 ONNX：external_data 里的 `offset` / `length` 是按 **fp32 的单位**
    记的（`length` 恰好等于 fp32 字节数），而文件本身只有一半大。也就是说
    「偏移 360960 处的 fp32 权重」在文件里其实是「偏移 180480 处的 fp16 权重」。

    判据只认文件大小：把该文件里所有 initializer 按 fp16 与按声明类型各算一次总字节，
    哪个等于真实文件大小就是哪个。这样不依赖命名、也不依赖 length 字段。
    """
    tot = {}  # path -> [按 fp16 算的字节, 按 data_type 算的字节]
    for init in model.graph.initializer:
        ext = _ext_map(init)
        if not ext:
            continue
        path, _ = _resolve(base_dir, ext["location"])
        n = _elems(init)
        esz = onnx.helper.tensor_dtype_to_np_dtype(init.data_type).itemsize
        e = tot.setdefault(path, [0, 0])
        e[0] += n * 2
        e[1] += n * esz

    out = {}
    for path, (b16, b32) in tot.items():
        real = os.path.getsize(path)  # 文件不存在就直接抛，早失败比猜着读强
        if real == b32:
            out[path] = 1.0
        elif real == b16 and b16 != b32:
            out[path] = 0.5
        else:
            raise SystemExit("%s 大小 %d 既不是 fp32 的 %d 也不是 fp16 的 %d，无法判断存储精度"
                             % (path, real, b32, b16))

    # 再校验一次「偏移也是按 fp32 记的」：把 offset 乘上 scale 之后，必须**严格连续**，
    # 且末尾正好等于文件大小。这是「数据是 fp16、偏移是 fp32」这个判断的唯一硬证据——
    # 单看数值分布看不出来（错位一个元素照样是"像权重"的浮点数）。
    cursor = {p: 0 for p in out}
    for init in model.graph.initializer:  # 必须按图里的顺序：导出时就是按这个顺序拼的
        ext = _ext_map(init)
        if not ext:
            continue
        path, _ = _resolve(base_dir, ext["location"])
        n = _elems(init)
        esz = onnx.helper.tensor_dtype_to_np_dtype(init.data_type).itemsize
        off = int(int(ext.get("offset", 0)) * out[path])
        size = n * (2 if out[path] == 0.5 else esz)
        if off != cursor[path]:
            raise SystemExit("%s 里的偏移不连续（%s：offset×scale=%d，期望 %d）"
                             % (os.path.basename(path), init.name, off, cursor[path]))
        cursor[path] += size
    for path, end in cursor.items():
        if end != os.path.getsize(path):
            raise SystemExit("%s 的累加字节 %d ≠ 文件大小 %d"
                             % (os.path.basename(path), end, os.path.getsize(path)))
    return out


def collect_models():
    """返回 [(label, onnx_path, ...)]。"""
    out = []
    for d in sorted(glob.glob(os.path.join(TTS_DIR, "*", "tts"))):
        name = os.path.basename(os.path.dirname(d))
        for p in sorted(glob.glob(os.path.join(d, "*.onnx"))):
            out.append((name + "/" + os.path.basename(p)[:-5], p))
    for p in sorted(glob.glob(os.path.join(HUBERT_DIR, "*.onnx"))):
        out.append(("hubert/" + os.path.basename(p)[:-5], p))
    return out


def filter_models(items, only):
    if not only:
        return items
    keys = [k.strip() for k in only.split(",") if k.strip()]
    hit = [it for it in items if any(k in it[0] for k in keys)]
    if not hit:
        sys.exit("没有匹配 --only 的模型：" + only)
    return hit


# ---------- inspect ----------

def cmd_inspect(args):
    for label, path in filter_models(collect_models(), args.only):
        m = onnx.load(path, load_external_data=False)
        base = os.path.dirname(path)
        scales = _file_scales(m, base)
        n_ext = n_half = 0
        for init in m.graph.initializer:
            ext = _ext_map(init)
            if not ext:
                continue
            n_ext += 1
            loc = ext["location"]
            fpath, _ = _resolve(base, loc)
            esz = onnx.helper.tensor_dtype_to_np_dtype(init.data_type).itemsize
            if scales[fpath] == 0.5 and esz == 4:
                n_half += 1
        print("== %s" % label)
        print("   onnx %.2f MB；initializer 走外部存储 %d 个，其中 **fp16 存储** %d 个"
              % (os.path.getsize(path) / 1e6, n_ext, n_half))
        for fpath, sc in scales.items():
            print("   %s：%.2f MB，实际按 %s 存"
                  % (os.path.basename(fpath), os.path.getsize(fpath) / 1e6,
                     "fp16" if sc == 0.5 else "fp32（与图声明一致）"))


# ---------- fp32 上采样 ----------

def cmd_fp32(args):
    for label, path in filter_models(collect_models(), args.only):
        t0 = time.time()
        m = onnx.load(path, load_external_data=False)
        base = os.path.dirname(path)
        scales = _file_scales(m, base)
        cache = {}          # 外部文件 → 整个读进来的 bytes（同一个 bin 被多个 initializer 共用，只读一次）
        n_up = 0
        for init in m.graph.initializer:
            ext = _ext_map(init)
            if not ext:
                continue
            loc = ext["location"]
            fpath, fellback = _resolve(base, loc)
            if fpath not in cache:
                with open(fpath, "rb") as f:
                    cache[fpath] = f.read()
                if fellback:
                    print("   ⚠ %s 不存在，改用 %s" % (loc, os.path.basename(fpath)))
            buf = cache[fpath]
            scale = scales[fpath]
            npt = onnx.helper.tensor_dtype_to_np_dtype(init.data_type)
            n = _elems(init)

            # 偏移与长度都按「图声明的类型」记；fp16 存储时整体除以 2（见 _file_scales）
            off = int(int(ext.get("offset", 0)) * scale)
            if scale == 0.5:
                if npt != np.float32:
                    raise SystemExit("%s/%s 是非 float32（%s）却按 fp16 存，暂不支持"
                                     % (label, init.name, npt.name))
                arr = np.frombuffer(buf, dtype=np.float16, count=n, offset=off)
                arr = arr.astype(np.float32).reshape(init.dims)
                n_up += 1
            else:
                raw = buf[off:off + n * npt.itemsize]
                if len(raw) != n * npt.itemsize:
                    raise SystemExit("%s/%s 数据越界" % (label, init.name))
                arr = np.frombuffer(raw, dtype=npt).reshape(init.dims)

            if not np.all(np.isfinite(arr)):
                raise SystemExit("%s/%s 上采样后出现 NaN/Inf，偏移算错了" % (label, init.name))
            init.raw_data = arr.tobytes()
            init.ClearField("external_data")
            init.data_location = onnx.TensorProto.DEFAULT

        out_dir = os.path.join(base, "fp32")
        os.makedirs(out_dir, exist_ok=True)
        out = os.path.join(out_dir, os.path.basename(path))
        onnx.save(m, out)
        print("   %s：上采样 %d 个 initializer → %s（%.2f MB，%.1fs）"
              % (label, n_up, os.path.relpath(out, ROOT), os.path.getsize(out) / 1e6, time.time() - t0))
        del m, cache


# ---------- int8 动态量化 ----------

def cmd_int8(args):
    from onnxruntime.quantization import QuantType, quantize_dynamic
    from onnxruntime.quantization.shape_inference import quant_pre_process

    for label, path in filter_models(collect_models(), args.only):
        base = os.path.dirname(path)
        src = os.path.join(base, "fp32", os.path.basename(path))
        if not os.path.exists(src):
            print("   %s：跳过（还没做 fp32，先跑 fp32）" % label)
            continue
        out_dir = os.path.join(base, "int8")
        os.makedirs(out_dir, exist_ok=True)
        out = os.path.join(out_dir, os.path.basename(path))
        t0 = time.time()

        # 必须先「预处理」再量化，否则几乎什么都量化不到：
        # 这些模型的线性层权重是以**转置形式**存的（`Transpose(常量) → MatMul`），
        # 而动态量化只认「B 直接是常量」的 MatMul（MatMulConstBOnly），于是 279MB 权重被整个跳过。
        # quant_pre_process 会做常量折叠，把 `Transpose(常量)` 折成一个转置好的常量，
        # 量化器这才看得见它们。它顺带做符号形状推断，也是量化所必需的。
        prep = os.path.join(out_dir, "_prep_" + os.path.basename(path))
        try:
            quant_pre_process(src, prep, skip_optimization=False,
                              skip_onnx_shape=False, skip_symbolic_shape=False, verbose=False)
        except Exception as e:  # noqa: BLE001
            print("   ⚠ %s 预处理失败，退回不折叠直接量化（效果会打折）：%s" % (label, e))
            prep = src
        try:
            quantize_dynamic(
                prep, out,
                per_channel=True,
                reduce_range=False,
                weight_type=QuantType.QInt8,
                op_types_to_quantize=["MatMul", "Gemm"],
                extra_options={"MatMulConstBOnly": True},
                use_external_data_format=True,
            )
        except Exception as e:  # noqa: BLE001 —— 现场报错信息比栈更有用
            print("   ✗ %s 量化失败：%s" % (label, e))
            continue
        if prep != src:
            for p in glob.glob(prep + "*"):  # 预处理产物只是中间件，别占着几百 MB
                os.remove(p)

        # quantize_dynamic 写外置数据时会留冗余：`.data` 差不多是模型实际引用的两倍
        # （hubert 实测 243 MB vs 引用 122 MB）。重新保存一次，按引用重写就能压回去。
        #
        # ⚠️ 重存之前必须**先把旧文件删掉**：onnx 的 save_external_data 是「追加」写入的，
        # 目标文件已存在就会把整份权重再追加一遍（实测把 122 MB 涨到 486 MB）。
        model = onnx.load(out)
        for p in glob.glob(out + "*"):
            os.remove(p)
        tmp = out + ".tmp"
        onnx.save_model(model, tmp,
                        save_as_external_data=True, all_tensors_to_one_file=True,
                        location=os.path.basename(out) + ".data", size_threshold=1024)
        os.replace(tmp, out)

        size = sum(os.path.getsize(p) for p in glob.glob(out + "*"))
        print("   %s：%.2f MB → %.2f MB（%.0f%%，%.1fs）"
              % (label, os.path.getsize(src) / 1e6, size / 1e6,
                 size / os.path.getsize(src) * 100, time.time() - t0))


# ---------- verify ----------

def _dummy_feeds(model, dim=64):
    """造一组"形状自洽"的假输入，只为了确认模型能加载、能跑完、不吐 NaN。

    符号维度统一填 dim：同一个符号在不同输入里出现时取值一样，形状关系（如 ref_seq 与
    ref_bert 的长度）就自然对得上。音频输入单独给长一点，否则 STFT 会报"窗比信号长"。
    """
    feeds = {}
    for inp in model.graph.input:
        tt = inp.type.tensor_type
        dtype = onnx.helper.tensor_dtype_to_np_dtype(tt.elem_type)
        audio = any(k in inp.name for k in ("ref_audio", "waveform", "input_values", "audio"))
        shape = []
        for d in tt.shape.dim:
            shape.append(d.dim_value if d.dim_value > 0 else (32000 if audio else dim))
        if np.issubdtype(dtype, np.integer):
            feeds[inp.name] = np.zeros(shape, dtype=dtype)
        else:
            feeds[inp.name] = (np.random.rand(*shape).astype(dtype) - 0.5)
    return feeds


def _cos(a, b):
    a = np.asarray(a, dtype=np.float64).reshape(-1)
    b = np.asarray(b, dtype=np.float64).reshape(-1)
    if a.size != b.size or a.size == 0:
        return float("nan")
    na, nb = np.linalg.norm(a), np.linalg.norm(b)
    return float(a @ b / (na * nb)) if na and nb else float("nan")


def cmd_verify(args):
    """加载 + 跑一遍假输入；若同时有 fp32 与 int8，再做一次数值 A/B。

    A/B 用**同一份输入**跑两遍、逐个输出算余弦：这是判断"int8 有没有把模型量化坏"的唯一凭据
    （听感要等 TTS 打通之后才有）。余弦对 stop_condition 这种标量没有意义（±1），
    统计里会一起算进去，所以看的是"中位"，别只看最低。
    """
    import onnxruntime as ort

    for label, path in filter_models(collect_models(), args.only):
        base = os.path.dirname(path)
        name = os.path.basename(path)
        cands = [p for p in (os.path.join(base, "int8", name),
                             os.path.join(base, "fp32", name)) if os.path.exists(p)]
        if not cands:
            # 还没转换过才去试原始的：GENIE 的原始 ONNX 引用的是 fp16 外部数据，
            # ORT 必然加载失败（这正是本工具存在的理由），别把它当成"转换出了问题"
            cands = [path]
        if not cands:
            print("   %s：没有可用的 onnx" % label)
            continue
        feeds = _dummy_feeds(onnx.load(cands[0], load_external_data=False))

        results = []
        for target in cands[:2]:
            t0 = time.time()
            try:
                sess = ort.InferenceSession(target, providers=["CPUExecutionProvider"])
                outs = sess.run(None, feeds)
            except Exception as e:  # noqa: BLE001
                print("   ✗ %s 加载/推理失败（%s）：%s" % (label, os.path.relpath(target, ROOT), e))
                continue
            bad = sum(0 if np.all(np.isfinite(o)) else 1 for o in outs if isinstance(o, np.ndarray))
            print("   %s %s（%s）%d 个输出，耗时 %.2fs，非有限输出 %d 个：%s"
                  % ("✓" if bad == 0 else "⚠", label, os.path.relpath(target, ROOT), len(outs),
                     time.time() - t0, bad, ", ".join(str(o.shape) for o in outs[:4])))
            results.append(outs)

        if len(results) == 2 and len(results[0]) == len(results[1]):
            cs = [_cos(a, b) for a, b in zip(results[0], results[1])]
            cs = [c for c in cs if not np.isnan(c)]
            if cs:
                low = sum(1 for c in cs if c < 0.99)
                print("      A/B：%d 个输出，余弦中位 %.4f / 最低 %.4f，低于 0.99 的有 %d 个"
                      % (len(cs), float(np.median(cs)), min(cs), low))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("cmd", choices=["inspect", "fp32", "int8", "verify"])
    ap.add_argument("--only", default="", help="按名字片段过滤，逗号分隔，如 t2s_encoder,vits")
    args = ap.parse_args()
    {"inspect": cmd_inspect, "fp32": cmd_fp32, "int8": cmd_int8, "verify": cmd_verify}[args.cmd](args)


if __name__ == "__main__":
    main()
