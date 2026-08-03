# mydocker — 自作コンテナランタイム（学習用）

Go の標準ライブラリだけで書いたコンテナランタイム。
`containerd` / `runc` / `libcontainer` / `moby` は**一切使っていない**。イメージの取得も自前で行う（`docker pull` を呼ばない）。

```
$ mydocker run alpine /bin/sh
/ # ps
PID   USER     TIME  COMMAND
    1 root      0:00 /bin/sh          ← 自分が PID 1
    7 root      0:00 ps
/ # hostname
mydocker                              ← ホストとは別のホスト名
/ # ls /
bin  dev  etc  home  lib  media ...   ← レジストリから取ってきた alpine の中身
```

---

## ⚠ 動く環境

**Linux でしか動かない。Windows でも macOS でもネイティブには一行も動かない。**
namespace / cgroup / overlayfs / pivot_root は Linux カーネルの機能そのものだからである。
さらに、これらを触るには**特権**（`CAP_SYS_ADMIN` など）が要る。

## 起動手順

```sh
# 1. 特権 Linux 開発環境に入る（「Docker を作るために Docker を使う」）
docker compose -f devcontainer/docker-compose.yml run --rm dev

# 2. その中でビルドする
go build -o mydocker ./cmd/mydocker

# 3. この環境で何が使えるかを確かめる（cgroup のバージョンは環境で違う）
./mydocker info

# 4. 動かす
./mydocker run alpine /bin/sh
```

E2E テスト:

```sh
python3 e2e_test.py
# レジストリのレイヤ配信ドメインに出られない環境では、届くレジストリを指定する:
MYDOCKER_TEST_IMAGE=mirror.gcr.io/library/alpine python3 e2e_test.py
```

---

## コマンド表

| コマンド | 説明 |
|---|---|
| `mydocker run [オプション] <イメージ> [コマンド...]` | コンテナを起動する。コマンド省略時はイメージの既定コマンド |
| `mydocker ps` | 実行中のコンテナを一覧する。末尾に `合計 N 件` を必ず出す |
| `mydocker exec <ID> <コマンド...>` | 実行中のコンテナの中でコマンドを実行する |
| `mydocker rm <ID>` | 終了したコンテナの痕跡（マウント・cgroup・記録）を片付ける |
| `mydocker pull <イメージ>` | イメージの取得だけを行う |
| `mydocker info` | この環境で使える資源制限の方式・overlay の可否・実行中件数を表示する |

`run` のオプション:

| オプション | 説明 |
|---|---|
| `--memory <量>` | メモリ上限（`10m` / `512k` / `1g` / バイト数） |
| `--cpus <数>` | CPU の上限（`0.5` で 0.5 コア分） |
| `--pids <数>` | プロセス数の上限 |
| `--hostname <名>` | コンテナのホスト名（既定 `mydocker`） |
| `--rootfs <パス>` | イメージの代わりにローカルのディレクトリを根として使う |
| `--layer <パス>` | 重ねるレイヤを直接指定する（下から順に複数回指定できる） |
| `--quiet` | 起動時のメッセージを出さない |

### 内部用サブコマンド（`--help` に出していない）

| コマンド | 説明 |
|---|---|
| `mydocker init <rootfs> <hostname> <workdir> <コマンド...>` | **利用者が直接叩くものではない。** 親プロセスが `/proc/self/exe` を使って自分自身を再実行するための入口（re-exec パターン）。手で実行すると「親との同期用ディスクリプタがありません」で止まる |
| `mydocker unpack <tar[.gz]> <展開先>` | tar の展開だけを単体で走らせる。E2E テストが細工した tar を投入して、展開先の外にファイルが作られないことを検査するために使う |

---

## 登場人物 ↔ コード 対応表

| やりたいこと | 何が起きるか | ソース |
|---|---|---|
| **自分をもう一度起動する** | `/proc/self/exe` を `init` という別の顔で起動し、その瞬間に名前空間を作る | `internal/container/run.go` の `Run` |
| **視界を区切る** | `Cloneflags` に UTS / PID / NS / IPC を並べる。`Unshareflags` でマウントの伝播を切る | `internal/container/run.go` の `Run` |
| **ホスト名を変える** | `syscall.Sethostname` | `internal/container/init.go` の `Init` |
| **根を挿げ替える** | 伝播を切る → 自分に bind mount → `pivot_root` → `chdir("/")` | `internal/container/init.go` の `pivotRoot` |
| **`ps` を正しく見せる** | `/proc` を貼り直す。**ここを忘れると名前空間が壊れて見える** | `internal/container/init.go` の `mountSpecialFilesystems` |
| **古い root を捨てる** | `MNT_DETACH` で切り離してから `/.old_root` を削除 | `internal/container/init.go` の `dropOldRoot` |
| **資源に上限をかける** | ディレクトリを作り、`memory.max`（v2）/ `memory.limit_in_bytes`（v1）に書く | `internal/cgroup/cgroup.go` の `New` |
| **上限を効かせる** | `cgroup.procs` に PID を書く。**作っただけでは効かない** | `internal/cgroup/cgroup.go` の `Apply` |
| **上限の後片付け** | `rmdir`。**`rm -rf` では消えない** | `internal/cgroup/cgroup.go` の `Destroy` |
| **イメージを取ってくる** | トークン → マニフェスト → レイヤ の3段。すべて `net/http` | `internal/image/registry.go` の `Store.Pull` |
| **アーキテクチャを選ぶ** | マニフェストリストから `linux/amd64` を選んで取り直す | `internal/image/registry.go` の `pickPlatform` |
| **レイヤを展開する** | `compress/gzip` + `archive/tar`。ホワイトアウトを overlay 形式に翻訳 | `internal/image/extract.go` の `ExtractLayer` |
| **展開先の外に書かせない** | 絶対パス・`..`・シンボリックリンク越えを弾く | `internal/image/extract.go` の `safeJoin` |
| **層を重ねる** | `lowerdir` を**逆順**に並べて `mount -t overlay` | `internal/overlay/overlay.go` の `New` |
| **アンマウントの安全** | 外れたことを確認するまで削除に進まない | `internal/overlay/overlay.go` の `Unmount` |
| **実行中コンテナを覚える** | `state.json`。PID + 起動時刻で生死を判定する | `internal/container/state.go` |
| **中でもう1つ動かす** | `setns` で PID / UTS / IPC に参加してから `init` を起動 | `internal/container/run.go` の `Exec` |
| **サブコマンドの振り分け** | 薄く保つ。仕事はしない | `internal/cli/cli.go` |

---

## ディレクトリ構成

```
src/
├── devcontainer/docker-compose.yml   特権 Linux 開発環境
├── e2e_test.py                       E2E テスト（Python 標準ライブラリのみ）
├── go.mod                            ★require は空。依存ライブラリなし
├── cmd/mydocker/main.go              入り口。薄い
└── internal/
    ├── cli/cli.go                    サブコマンドの振り分け
    ├── container/
    │   ├── run.go                    親プロセス側。re-exec と後片付け、exec
    │   ├── init.go                   ★子プロセス側。pivot_root → mount → exec
    │   ├── state.go                  実行中コンテナの記録（ps / exec 用）
    │   └── setns_linux_*.go          setns のシステムコール番号（arch 別）
    ├── cgroup/cgroup.go              cgroup v2 優先・v1 フォールバック
    ├── image/
    │   ├── registry.go               Docker Registry API v2
    │   └── extract.go                tar.gz の展開・ホワイトアウト・パス検証
    └── overlay/overlay.go            OverlayFS のマウントとアンマウント
```

状態の置き場は `/var/lib/mydocker/`:

```
/var/lib/mydocker/
├── layers/<digest>/            展開済みレイヤ（読み取り専用・コンテナ間で共有）
├── layers/<digest>.complete    ★展開が最後まで終わった印
├── manifests/<参照>.json       マニフェストのキャッシュ（2回目は通信しない）
├── configs/<digest>.json       イメージ設定（Env / Cmd / WorkingDir）
└── containers/<ID>/
    ├── state.json              実行中コンテナの記録
    ├── merged/                 overlay の結果 ＝ コンテナの /
    ├── upper/                  書き込みはここに溜まる（捨てれば元通り）
    └── work/                   overlayfs の作業場所
```

---

## ⚠ 実装していないもの

- **ネットワークの隔離**（`CLONE_NEWNET` / veth / bridge / NAT）。
  コンテナはホストのネットワーク名前空間をそのまま共有している。
  コンテナの中で `ip addr` を打つとホストの `eth0` がそのまま見える。
  理由と、実装するなら何が必要かは `docs/08-network.html` に書いた
- イメージのビルド（Dockerfile 相当）／レジストリへの push
- rootless 実行（user namespace）／seccomp・capabilities の絞り込み
- ボリューム管理・複数コンテナのオーケストレーション

## ⚠ 既知の割り切り

- `exec` は PID / UTS / IPC 名前空間には `setns` で参加するが、**マウント名前空間だけは新しく作る**。
  Go のランタイムはマルチスレッドで、マウント名前空間への `setns` は
  シングルスレッドのプロセスしか許されないため（`EINVAL`）。
  同じ overlay の `merged` を根にするので、見えるファイルは完全に同一である。
  詳細は `../03_implementation.md` の判断 7 を参照
