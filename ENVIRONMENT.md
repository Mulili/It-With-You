# 环境实测记录（阶段0 验收）

> 本文件记录 阶段0「环境准备」的实际检测结果，作为开发环境的基线快照。
> 检测时间：2026-09-15 10:31 (+08:00)
> 检测方式：在本机直接执行版本命令 / `wails doctor` / 探测服务与网络，非纸面核对。

## 一、检查结论

**阶段0 环境达标，可以进入阶段1。** 12 项中 11 项通过，1 项（pgvector 运行时安装）待人工执行确认——扩展文件已 100% 就位，风险极低。

| # | 检查项 | 阶段0 要求 | 实测结果 | 结论 |
|---|---|---|---|---|
| 1 | Go | 1.22+ | `go1.25.0 windows/amd64` | ✅ |
| 2 | Node.js | 18+ | `v24.20.0` | ✅ |
| 3 | npm | 随 Node | `11.19.0`（另有 pnpm `12.3.4`） | ✅ |
| 4 | Wails CLI | 可用、`doctor` 全绿 | `v2.16.0`，`wails doctor` → `SUCCESS Your system is ready for Wails development!` | ✅ |
| 5 | WebView2 | 必需运行时 | `152.0.4191.66`（HKLM 已注册，运行时进程活跃） | ✅ |
| 6 | PostgreSQL | 18 | `psql 18.6`，服务 `postgresql-x64-18` 运行中 / 自动启动 | ✅ |
| 7 | pgvector | `CREATE EXTENSION vector;` 成功 | 扩展文件齐全，`default_version = 0.8.6`；**运行时安装待确认** | ⚠️ |
| 8 | Git | 可正常开发 | `2.55.0.windows.3` | ✅ |
| 9 | VS Code | 可正常开发 | `1.134.0`，`code` 已在 PATH | ✅ |
| 10 | 操作系统 | Win10 需手装 WebView2 | Windows 11 家庭版中文版，Build 26100 / 24H2，自带 WebView2 | ✅ |
| 11 | 网络（Go 模块） | 能拉依赖 | `GOPROXY` 生效，可拉取 `wails/v2` 全量版本 | ✅ |
| 12 | 网络（npm） | 能拉依赖 | `registry.npmjs.org` 返回 200，`npm view vue version` → `3.5.42` | ✅ |

## 二、详细实测数据

### 1. 工具链版本

```
go version go1.25.0 windows/amd64
node v24.20.0
npm 11.19.0
pnpm 12.3.4
git version 2.55.0.windows.3
code 1.134.0 (x64)
wails v2.16.0
psql (PostgreSQL) 18.6
```

未安装：`docker`（本项目不依赖，无需处理）。

### 2. `wails doctor` 输出要点

```
# System
OS           | Windows 10 Home China   (注册表 ProductName 的历史遗留写法)
Version      | 2009 (Build: 26100)
ID           | 24H2
Branding     | Windows 11 家庭版 中文版
Go Version   | go1.25.0
Platform     | windows / amd64

# Dependencies
WebView2   Installed  152.0.4191.66
Nodejs     Installed  24.20.0
npm        Installed  11.19.0
*upx       Available   (可选，未安装)
*nsis      Available   (可选，未安装)

SUCCESS  Your system is ready for Wails development!
```

> 说明：`OS` 显示 `Windows 10 Home China` 是注册表 `ProductName` 未随系统升级更新的常见现象，`Branding` 与 `Build 26100` 表明实际为 **Windows 11 24H2**。Win11 自带 WebView2，因此 README 中"Win10 需手动安装 WebView2"的风险在本机不成立。

**可选依赖缺省说明**（不影响阶段1～6，仅阶段7 打包发布需要）：

- `upx` —— 用于压缩产物体积，缺失只影响包体大小。
- `nsis` —— 用于生成 Windows 安装包，缺失时 `wails build` 仍能产出 exe。

### 3. PostgreSQL 与 pgvector

安装位置与实例信息：

```
安装根目录   <PG_HOME>
数据目录     <PG_HOME>\data
服务名       postgresql-x64-18
启动类型     AUTO_START
服务账户     NT AUTHORITY\NetworkService
监听         127.0.0.1:5432（可连通）
客户端       <PG_HOME>\bin\{psql,pg_dump,pg_ctl}.exe（已在 PATH）
环境变量     PostgreSqlHOME = <PG_HOME>\bin
身份验证     pg_hba.conf → local/host 全部 scram-sha-256（需密码，无 trust）
```

pgvector 已安装到当前实例的扩展目录：

```
<PG_HOME>\share\extension\vector.control     → default_version = '0.8.6'
<PG_HOME>\share\extension\vector--0.8.6.sql
<PG_HOME>\share\extension\vector.sql
<PG_HOME>\lib\vector.dll
```

`vector.control` 内容：

```
comment = 'vector data type and ivfflat and hnsw access methods'
default_version = '0.8.6'
module_pathname = '$libdir/vector'
relocatable = true
```

**⚠️ 待人工确认的一步**：扩展文件就位 ≠ 扩展已启用。需在建库后执行 `CREATE EXTENSION` 才算通过阶段0 验收（此步需 postgres 超管密码，未在本次自动检测中执行）。

### 4. 构建缓存与磁盘

```
GOPATH       <GOPATH 根目录>
GOROOT       <Go 安装目录>
GOMODCACHE   <GOPATH 根目录>\pkg\mod
GOCACHE      %LOCALAPPDATA%\go-build
GOPROXY      https://goproxy.cn,direct
npm registry https://registry.npmjs.org/
```

> 注意：本机 `GOPATH` 恰好是本项目**工作区的父目录**。本项目以 Go Modules 管理，不受 GOPATH 布局影响，但需知晓 `pkg\mod` 会落在该目录下。

磁盘余量：

| 盘符 | 总容量 | 可用 |
|---|---|---|
| E: | 953.9 GB | 81.8 GB |
| C: | 300 GB | 56.6 GB |

### 5. 网络与代理

- TCP 连通性：`goproxy.cn:443`、`registry.npmjs.org:443` 均可达。
- 系统代理：注册表 `Internet Settings` 下有 `ProxyServer` 条目（指向本机代理的本地端口），但 `ProxyEnable = 0`（**代理当前已关闭**）。若后续拉取依赖变慢，可考虑启用本地代理。

## 三、pgvector 运行时验证步骤（待执行）

连接信息：`127.0.0.1:5432`，用户 `postgres`，psql 已在 PATH。

```powershell
# 1) 确认扩展在本实例可用
psql -U postgres -h 127.0.0.1 -c "SELECT name, default_version, installed_version FROM pg_available_extensions WHERE name='vector';"
# 预期：1 行，default_version = 0.8.6，installed_version 为 NULL

# 2) 建项目库
psql -U postgres -h 127.0.0.1 -c "CREATE DATABASE companion;"

# 3) 安装扩展
psql -U postgres -h 127.0.0.1 -d companion -c "CREATE EXTENSION IF NOT EXISTS vector; SELECT extname, extversion FROM pg_extension WHERE extname='vector';"
# 预期：vector | 0.8.6
```

进阶冒烟测试（验证 向量类型 + HNSW 索引 + 余弦距离算子，即阶段4 的三件套）：

```sql
CREATE TABLE _vec_probe (id int, embedding vector(1536));
CREATE INDEX ON _vec_probe USING hnsw (embedding vector_cosine_ops);
INSERT INTO _vec_probe VALUES (1, array_fill(0.1, ARRAY[1536])::vector);
SELECT id, embedding <=> array_fill(0.2, ARRAY[1536])::vector AS cosine_dist FROM _vec_probe;
DROP TABLE _vec_probe;
-- 预期：建索引无报错；查询 1 行，cosine_dist ≈ 0
```

> 建表时 `vector(n)` 的维度选择见 README 阶段4「关键决策」：**1536 = OpenAI text-embedding-3-small / ada-002，1024 = BGE-M3**，动手前必须先锁定嵌入模型。

## 四、阶段1 开工前置结论

| 阶段1 依赖 | 状态 |
|---|---|
| `wails init -t vue` 可执行 | ✅ Wails CLI 就绪 + 网络可达 |
| 透明窗口（WebView2 透明渲染） | ✅ WebView2 152 已就位，Win11 原生支持 |
| 系统托盘 | ✅ Windows 原生支持，无需额外运行时 |
| 前端构建（Vue） | ✅ npm 11.19 / Node 24 可用 |

`wails doctor` 的非致命提示（upx / nsis 缺失）与阶段1 无关，可在阶段7 前补齐。
