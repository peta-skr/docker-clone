# CLAUDE.md — 自作コンテナランタイム mydocker（学習用）+ 教材サイト

## ① プロジェクト概要

`docker run` の中で何が起きているかを、**自分で書いて理解する**ためのプロジェクト。Go の標準ライブラリだけでコンテナランタイム `mydocker` を実装し、その実装を読み解く教材サイト（静的HTML・全11ページ）を付ける。中心命題は「**コンテナは軽量な仮想マシンではない**」——実体は namespace で視界を区切り、cgroup で資源を締め、pivot_root で根を挿げ替えた**ただのプロセス**である。学習目的のため、**市場検証・収益化はスキップ**している。

| ファイル | 役割 |
|---|---|
| `00_smoke_test.md` | 疎通実験の記録。**実際のコマンド出力を貼ってある**。スコープはここで確定した |
| `mydocker-requirements.md` | 要件定義書。FR-1〜FR-10 と非機能要件 |
| `03_implementation.md` | 実装ログ。設計判断（判断軸/対抗案/結果）・踏んだバグ・自己レビューの結果 |
| `CLAUDE.md` | このファイル。プロジェクト憲章 |
| `index.html` | `docs/index.html` へのリダイレクトのみ |
| `docs/` | 教材サイト（序章 + 第00〜09章 = 全11ページ） |
| `docs/_引き継ぎ指示.md` | 章立て表と進捗チェックリスト |
| `src/` | `mydocker` の実装本体 |
| `src/e2e_test.py` | E2E テスト。Python 標準ライブラリのみ。**56 項目** |
| `src/devcontainer/docker-compose.yml` | 特権 Linux 開発環境 |

## ② 技術スタック・構成

| 層 | 技術 | 場所 |
|---|---|---|
| ランタイム本体 | Go 1.24（**標準ライブラリのみ**。`go.mod` の require は空） | `src/cmd/`, `src/internal/` |
| 名前空間・根の挿げ替え | `syscall`（`Cloneflags` / `PivotRoot` / `Mount` / `Sethostname` / `Exec`） | `src/internal/container/` |
| 資源制限 | cgroup v2 優先・v1 フォールバック（ファイル操作のみ） | `src/internal/cgroup/` |
| イメージ取得 | Docker Registry HTTP API v2（`net/http`。`docker pull` を呼ばない） | `src/internal/image/registry.go` |
| レイヤ展開 | `archive/tar` + `compress/gzip`、ホワイトアウト翻訳、パス検証 | `src/internal/image/extract.go` |
| 層の重ね合わせ | OverlayFS（`syscall.Mount`） | `src/internal/overlay/` |
| E2E テスト | Python 3 標準ライブラリのみ（`subprocess` / `tarfile`） | `src/e2e_test.py` |
| 開発環境 | 特権 Docker コンテナ（`golang:1.24-bookworm`, `privileged: true`, `cgroup: host`） | `src/devcontainer/` |
| 教材サイト | 静的 HTML + 1枚の CSS。**JavaScript は一切使わない**（FAQ は `<details>`） | `docs/` |

**⚠ 動作環境**: **Linux 専用**。namespace / cgroup / overlayfs / pivot_root はいずれも Linux カーネルの機能そのものなので、**Windows でも macOS でもネイティブには一行も動かない**。さらにこれらを触るには**特権**（`CAP_SYS_ADMIN` を含む）が要る。通常のユーザ権限では `mount` も `pivot_root` も `EPERM` で落ちる。

## ③ 動かし方

**⚠ まずこれを読むこと: Windows / macOS では動かない。** 開発も実行も、下の 1 行で入る**特権 Linux コンテナの中**で行う。「Docker を作るために Docker を使う」入れ子になるが、これが最も再現性が高い。

```sh
docker compose -f src/devcontainer/docker-compose.yml run --rm dev   # 特権 Linux 開発シェルに入る。privileged が無いと mount も cgroup も EPERM で落ちる
go build -o mydocker ./cmd/mydocker                                  # ここでビルド。依存が無いので go get は不要（go.mod の require は空のまま）
./mydocker info                                                      # ★先にこれを見る。cgroup が v2 か v1 か環境で違い、できることが変わる
./mydocker run alpine /bin/sh                                        # 起動。中で ps を打つと自分が PID 1 だけ並ぶ＝隔離できている証拠
python3 e2e_test.py                                                  # 受け入れ条件の機械検査。末尾に「N passed, M failed」を実数で出す
```

⚠ レジストリのレイヤ配信ドメイン（`production.cloudfront.docker.com`）に出られない環境では、`MYDOCKER_TEST_IMAGE=mirror.gcr.io/library/alpine` のように**届くレジストリを指定する**。`mydocker` は任意のレジストリを扱える。

## ④ 主要な設計判断（詳細は 03_implementation.md）

- **名前空間の作り方**: `unshare` して `exec` する素朴案ではなく、`/proc/self/exe` を `init` という別サブコマンドで起動する（**re-exec パターン**）。⚠ **PID 名前空間は `unshare` を呼んだプロセス自身には適用されない**ため、素朴案では自分が PID 1 になれない。`clone` のフラグとして渡せば、子は生まれた瞬間から PID 1 である。
- **親子の同期**: 起動してすぐ走らせず、**パイプ（fd 3）で合図を待たせる**。⚠ これが無いと cgroup へ登録する前に子が走り出し、上限テストが気まぐれに通ったり落ちたりする。合図を送らずに閉じることで「親が失敗した」も伝えられる。
- **根の挿げ替え**: `chroot` ではなく **`pivot_root`**。`chroot` は「`/` の見え方」を変えるだけで、**プロセスが既に持っている外側への参照を無効化しない**ので、fd を掴んでから `fchdir` で抜け出せる（実際に脱獄を再現して確認した）。`pivot_root` は古い root を `MNT_DETACH` でマウント木ごと切り離す。
- **資源制限**: cgroup **v2 優先・v1 フォールバック**の2バックエンド。⚠ 判定は「cgroup2 がマウントされているか」ではなく「**`cgroup.controllers` に `memory` が語として見えるか**」で行う。hybrid 構成では v2 があっても `memory` は v1 側に取られており、v2 だけの実装は静かに失敗する。
- **イメージ取得**: `docker pull` を呼ばず **Registry HTTP API v2 を直叩き**（トークン → マニフェスト → レイヤ）。⚠ マニフェストリストからの選択は `platform.os`/`architecture` で**明示的に絞る**。「最初の項目」は attestation（署名の付随物）のことがあり確実に壊れる。
- **ホワイトアウト**: 全レイヤを平坦化して削除するのではなく、**overlayfs が理解する形に翻訳**する（`.wh.X` → キャラクタデバイス 0:0、`.wh..wh..opq` → `trusted.overlay.opaque` 拡張属性）。平坦化するとレイヤが特定イメージ専用になり、digest によるキャッシュ共有が効かなくなるため。
- **tar 展開の安全**: 絶対パス・`..` に加えて、**親ディレクトリの実体がシンボリックリンクで外へ抜けていないか**まで確認する。前2つだけでは「`foo -> /` を作ってから `foo/etc/passwd` に書く」手口が通る。
- **`exec` の実装**: PID/UTS/IPC は `setns` で参加し、**マウント名前空間だけは新しく作って同じ `merged` を根にする**。⚠ マウント名前空間への `setns` はシングルスレッドのプロセスしか許されず（`EINVAL`）、Go は常にマルチスレッド。`runc` はこのために cgo を使うが、この教材は標準ライブラリのみという前提を守った。既知の割り切りとして README に明記。
- **後片付けの順序**: 「**アンマウント → 削除**」を絶対に崩さない。⚠ `umount` は `EBUSY` で日常的に失敗し、そこで削除に進むと**マウント越しにイメージの実体まで消える**。`Unmount` は成功後も `IsMountPoint` で再確認し、呼び出し元は失敗したら削除に進まない。cgroup は `rm -rf` では消えないので **`rmdir`**。
- **終了コード**: シグナル死は **128+シグナル番号**に翻訳する。Go の `ExitCode()` はシグナル死で -1 を返し、そのままだと 255 になって「OOM で殺された」ことが伝わらない。

## ⑤ 落とし穴（このプロジェクト固有・実際に踏んだものだけ）

- **`stat -fc %T /sys/fs/cgroup/` が `tmpfs` を返す** →（v1/v2 が同居する **hybrid 構成**。tmpfs の置き場の下に v1 のコントローラが個別にマウントされ、`unified/` に v2 もある）→ `mount | grep cgroup` まで見て判断する。**この環境がまさにこれだった**。
- **cgroup2 はマウントできるのに `memory.max` が作られない** →（v2 の `cgroup.controllers` が `cpuset hugetlb` だけ。`memory`/`cpu`/`pids` は v1 に取られている。コントローラは v1/v2 のどちらか一方にしか結び付かない）→ `cgroup.controllers` を**語単位で**読んでバックエンドを選ぶ。`strings.Contains` は `memory` が別の語に誤ヒットするので使わない。
- **`cat memory.limit_in_bytes` が期待値を返すのに上限が効かないように見える** →（**テストのほうが間違っていた**。`head -c 100M /dev/zero | wc -c` はストリームを流すだけでメモリを溜めない）→ `dd if=/dev/zero of=/dev/null bs=100M count=1` のように**一度に大きく確保**させ、**上限なしの対照とペアで**走らせる。
- **`ps` がホストのプロセスを 87 個表示する（ホストは 85 個）** →（PID 名前空間は**効いている**。`/proc` がホストのものを指したままなだけ）→ `pivot_root` 後に `/proc` を貼り直す。**「namespace が壊れている」と誤診する典型**。
- **`pivot_root` が `EINVAL` で落ちる** →（新しい root が**マウントポイントでない**／マウントの伝播を切っていない）→ `mount("", "/", MS_PRIVATE|MS_REC)` の後に **rootfs を自分自身に bind mount** する。
- **マニフェストに `layers` が無い** →（返ってきたのは**マニフェストリスト**）→ `platform` で絞って digest で取り直す。**そのとき `architecture: unknown` の attestation を掴まないこと**。
- **`setns` が `EINVAL`（マウント名前空間）** →（Go のランタイムがマルチスレッド。マウント名前空間への `setns` はシングルスレッドのプロセスしか許されない）→ この教材では参加を諦め、同じ `merged` を根にする方式にした。
- **OOM で殺したのに終了コードが 255** →（Go の `ExitError.ExitCode()` はシグナル死で -1 を返す）→ `WaitStatus.Signaled()` を見て 128+シグナル番号を返す。
- **cgroup の親ディレクトリだけ残る** →（葉は消しているが親を消していない）→ `Destroy` で親も `rmdir`。他のコンテナが動いていれば `ENOTEMPTY` で失敗するが、それが正しい。
- **`mydocker ps` の出力を `awk '{print $4}'` で解析すると壊れる** →（COMMAND 列に空白が入る。`sleep 40` の `40` を掴む）→ 機械が読むときは `state.json` を直接読む。
- **`production.cloudfront.docker.com` へ出られない環境がある** →（組織の egress ポリシー。マニフェストは取れるのに**レイヤの取得だけ 403**）→ `mydocker` は任意のレジストリを扱えるので、`mirror.gcr.io/library/alpine` のように**届くレジストリを指定する**。回避策を実装に埋め込まない。

## ⑥ 教材サイトの編集方針（docs/ 版）

**章構成**: 序章 + 第00〜09章 = 全11ページ。層を下から積む構成で、**1章飛ばすと次章は必ず分からない**ことを序章で告げる。

| ページ | 章 | 担当 src ファイル |
|---|---|---|
| `index.html` | 序 はじめに・準備 | — |
| `00-env.html` | 00 Linux の中に入る | `devcontainer/docker-compose.yml`, `go.mod` |
| `01-reexec.html` | 01 自分をもう一度起動する | `container/run.go`, `container/init.go` |
| `02-namespace.html` | 02 見える世界を区切る | `container/run.go`（Cloneflags）, `container/init.go`（hostname） |
| `03-pivotroot.html` | 03 根を挿げ替える | `container/init.go`（pivot_root・/proc） |
| `04-cgroup.html` | 04 資源に上限をかける | `cgroup/cgroup.go` |
| `05-registry.html` | 05 イメージを取ってくる | `image/registry.go` |
| `06-overlay.html` | 06 層を重ねる | `image/extract.go`, `overlay/overlay.go` |
| `07-cli.html` | 07 道具にする | `cli/cli.go`, `container/state.go`, `cmd/mydocker/main.go` |
| `08-network.html` | 08 外とつながる | **未実装のため「なぜ難しいか」の章**（`network/network.go` は作らない） |
| `09-beyond.html` | 09 さらに先へ | 発展課題＋全体総括 |

**規約**:

- **インライン CSS 禁止。** `style=` 属性を書かない。すべて `docs/style.css` のクラスで表現する
- **JavaScript を一切使わない。** FAQ・理解度チェックの開閉は `<details>` の標準機能
- **アクセント色は青系**（`--accent:#2a6f97`）。コンテナ／低レイヤの教材なので。それ以外の CSS 値は触らない
- **教材に出す数値・コマンド出力は実物と整合させる。** 憶測で書かない。実測値の出所は `00_smoke_test.md` と `03_implementation.md`
- **掲載コードを変えたら該当章も同期する（一字一句一致）。** 省略は `// …（後のステップで）` と明示する。関数名・型名が `src/` に実在するかを照合してから公開する
- **実装できなかったものの章を「できたふり」で書かない。** ネットワークは未実装なので、08 章は「なぜ着手しなかったか・何が必要になるか・どこが難所か」を書く章にする
- `aside.spine` は全11ページで完全一致させ、`current` の位置だけを動かす
- `pager`（前へ／次へ）を全章で正しく連結する

## ⑦ 収益化モデル

なし（学習目的）。成果は技術記事・note の素材として転用する。
