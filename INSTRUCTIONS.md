# docker-clone — 着手指示書

**この教材はまだ何も作られていない（このファイルだけがある状態）。**
このファイル**だけ**を読んで、自作コンテナランタイムの実装と教材サイトを一から作り切ること。
他プロジェクトを参照する必要はない。必要な型・規約はすべてこの文書に書いてある。

---

## 0. 何を作るか

**Docker のクローン — 自作のコンテナランタイム `mydocker`** を Go で実装し、
その実装を読み解く**教材サイト**（静的HTML・全11ページ）を付ける。**学習目的**であり、収益化はしない。

目指す到達点は、これが動くこと:

```
# mydocker run alpine /bin/sh
/ # ps
PID   USER     TIME  COMMAND
    1 root      0:00 /bin/sh          ← 自分が PID 1
    5 root      0:00 ps
/ # hostname
mydocker                              ← ホストとは別のホスト名
/ # ls /
bin  dev  etc  home  proc  root ...   ← Docker Hub から取ってきた alpine の中身
```

**「コンテナは軽量な仮想マシンではない」**——実体は namespace で視界を区切り、cgroup で資源を締め、
pivot_root で根を挿げ替えた**ただのプロセス**である、ということを手を動かして理解するのが目的。

### ⚠ この教材はライブラリを使わない

`containerd` / `runc` / `libcontainer` / `moby` などのコンテナ関連ライブラリは**一切使わない**。
Go の標準ライブラリ（`syscall` / `os/exec` / `net/http` / `archive/tar` / `compress/gzip`）だけで書く。

**イメージの取得も自作する**（`docker pull` を呼ばない）。Docker Registry HTTP API v2 を直接叩く。

---

## 1. ⚠ 最初にやること — 環境の疎通実験（30〜60分）

**この教材の最大のリスクは技術ではなく開発環境である。**
コンテナランタイムは Linux カーネルの機能（namespace / cgroup / overlayfs / pivot_root）そのものなので、
**Windows でも macOS でもネイティブには一行も動かない。**

### 開発環境の方針

**Linux の中で開発する。** 手段は2つあり、**先に疎通実験で確かめてから決める**。

| 案 | 方法 | 懸念 |
|---|---|---|
| **A. 特権 Docker コンテナ（推奨）** | `ubuntu:24.04` を `--privileged` で起動し、その中で開発・実行する | 「Docker を作るために Docker を使う」入れ子。ただし環境が固定できて再現性が高い |
| **B. WSL2 の Ubuntu** | WSL2 上で直接開発する | cgroup v2 の有効/無効やコントローラの利用可否が環境によって違う。**確かめずに前提にしない** |

**A を既定とする。** `src/devcontainer/` に `docker-compose.yml` を置き、`docker compose run --rm dev` で
開発シェルに入れるようにする。コンパイルもテストも実行もその中で行う。

⚠ 教材の 00 章は、この「Linux の中に開発環境を作る」ところから始めること。読者も同じ壁に必ずぶつかる。

### 疎通実験（実装を1行も書く前にやる）

以下を**実際にコマンドを打って確かめ**、結果を `00_smoke_test.md` に記録する。
**推測で「動くはず」と書かない。出力をそのまま貼る。**

```sh
# 1. カーネルのバージョンと cgroup のバージョン
uname -r
stat -fc %T /sys/fs/cgroup/          # cgroup2fs なら v2、tmpfs なら v1

# 2. namespace が作れるか
unshare --pid --fork --mount-proc ps aux    # 自分が PID 1 に見えるか

# 3. cgroup v2 のコントローラが使えるか
cat /sys/fs/cgroup/cgroup.controllers        # memory / cpu / pids が並ぶか
cat /sys/fs/cgroup/cgroup.subtree_control

# 4. cgroup を実際に作って上限を書けるか
mkdir /sys/fs/cgroup/smoketest && echo 10000000 > /sys/fs/cgroup/smoketest/memory.max && cat /sys/fs/cgroup/smoketest/memory.max

# 5. overlayfs がマウントできるか
mkdir -p /tmp/ovl/{lower,upper,work,merged} && mount -t overlay overlay -o lowerdir=/tmp/ovl/lower,upperdir=/tmp/ovl/upper,workdir=/tmp/ovl/work /tmp/ovl/merged && mount | grep overlay

# 6. Docker Hub からトークンが取れるか（イメージ取得の下見）
curl -s "https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/alpine:pull" | head -c 200
```

### ⚠ 疎通実験の後片付け（必ずやる）

**上の 4 と 5 は環境に痕跡を残す。** cgroup ディレクトリは残り、overlay は**マウントされたまま**になる。
**この教材は「後片付けができているか」を検査する教材である。** その入口の疎通実験が汚したまま終わるのでは筋が通らない。

実験のあと必ず実行し、**片付いたことを確認した出力も `00_smoke_test.md` に貼る**:

```sh
# 5 の後片付け。★アンマウントが成功したときだけ削除する
umount /tmp/ovl/merged

# 「本当に外れたか」を確認してからでなければ消さない。
# mountpoint が 0（＝まだマウント中）を返すなら、削除せず手で調べる。
if mountpoint -q /tmp/ovl/merged; then
	echo "!! まだマウントされている。削除しない。lsof や fuser で掴んでいるプロセスを調べること"
else
	rm -rf /tmp/ovl
fi

# 4 の後片付け（中にプロセスが残っていると rmdir は失敗する）
rmdir /sys/fs/cgroup/smoketest

# 片付いたことの確認 — どちらも何も出力されなければ成功
mount | grep -c /tmp/ovl        # → 0
ls -d /sys/fs/cgroup/smoketest  # → No such file or directory
```

⚠ **`umount` の成否を確かめずに `rm -rf` を続けてはいけない。** アンマウントは
**「デバイスが使用中（`EBUSY`）」で日常的に失敗する**——コンテナのプロセスが残っていたり、
シェルの作業ディレクトリがその中だったりするだけで起きる。
そこで `rm -rf` が走ると、**マウント越しに `lower` の実体（＝イメージの中身）まで消える。**
上の例のように `mountpoint` で確認してから消すこと。**`&&` で繋ぐだけでもよいが、
`;` や改行で並べるのは危険**。

⚠ **`rm -rf` でマウントそのものを消そうとしない。** 必ず `umount` が先。

⚠ **cgroup ディレクトリは `rm -rf` では消えない。** cgroupfs は通常のファイルシステムではないので、
**`rmdir` を使う**（中が空でなければ失敗する。それが正しい挙動）。

**この後片付けの手順は、そのまま 07 章「道具にする」の後片付け実装の下敷きになる。**
疎通実験で自分の手で片付けておくと、実装すべきことが体で分かる。

### 判定と縮退

疎通実験の結果に応じて**スコープを決める**。ここで無理をすると後半が全部溶ける。

| 確認項目 | 通ったら | 落ちたら |
|---|---|---|
| namespace（2） | **Must** のまま進む | **ここが落ちたら教材が成立しない。** 環境を A 案に切り替えて再実験 |
| cgroup v2（3, 4） | cgroup を **Must** | cgroup を **Should** に降格し、教材04章は「なぜ効かない環境があるか」を書く章にする |
| overlayfs（5） | 層の重ね合わせを **Should** | **Could に降格**。単一 rootfs の展開だけで進める |
| Registry（6） | イメージ取得を **Should** | ローカルの rootfs tarball を使う方式のみにする |

**縮退した場合は、何をどの理由で落としたかを `00_smoke_test.md` と最終報告に必ず書く。**

---

## 2. 優先順位（時間切れのときの判断基準）

**実装（`src/`）> 教材（`docs/`）。** 教材は後から章ごとに書けるが、動かない実装は資産にならない。

| 優先度 | 内容 |
|---|---|
| **Must（絶対に完成させる）** | 疎通実験／開発環境／**namespace 隔離**（PID・UTS・MNT・IPC）／**pivot_root で根の挿げ替え**／`/proc` の再マウント／ローカル rootfs で `mydocker run <cmd>` が動く／cgroup で**メモリ上限が実際に効く**こと |
| **Should** | Docker Hub からのイメージ取得（Registry API v2）／OverlayFS による層の重ね合わせ／`ps` 相当（実行中コンテナの一覧）／`exec` |
| **Could（削ってよい）** | **ネットワーク（veth + bridge + NAT）**／`rm` / `logs`／rootless（user namespace）／seccomp・capabilities |

⚠ **ネットワークは Could に置いてある。** veth ペアの作成・bridge への接続・iptables による NAT は
それ単体で数時間かかるうえ、失敗が分かりにくい。**Must と Should が終わるまで手を出さない。**

Must が終わらないうちに Should へ進まない。削ったものは最終報告に明記する。

---

## 3. 成果物とディレクトリ構成

```
docker-clone/
├── INSTRUCTIONS.md                 # このファイル
├── 00_smoke_test.md                # ★疎通実験の結果（出力を貼る）
├── mydocker-requirements.md        # 要件定義書（§5 の型で書く）
├── 03_implementation.md            # 実装ログ・設計判断・踏んだバグ
├── CLAUDE.md                       # プロジェクト憲章（§9 の7項目テンプレート）
├── index.html                      # docs/index.html へのリダイレクトのみ
├── docs/                           # 教材サイト（序章 + 第00〜09章 = 全11ページ）
│   ├── index.html
│   ├── 00-env.html … 09-beyond.html
│   ├── style.css                   # §11 に全文あり。コピーして色だけ変える
│   └── _引き継ぎ指示.md            # 章立て表と進捗チェックリスト
└── src/
    ├── devcontainer/
    │   └── docker-compose.yml      # ★特権 Linux 開発環境
    ├── README.md                   # 起動手順・コマンド表・「登場人物↔コード」対応表
    ├── e2e_test.py                 # §8 の型。Python標準ライブラリのみ
    ├── go.mod
    ├── cmd/mydocker/main.go        # CLI 入口。薄く保つ
    └── internal/                   # 関心事ごとに切る（レイヤで切らない）
        ├── cli/cli.go              # サブコマンドの振り分け
        ├── container/
        │   ├── run.go              # 親プロセス側。namespace 付きで自分を再実行
        │   ├── init.go             # ★子プロセス側。pivot_root → mount → exec
        │   └── state.go            # 実行中コンテナの記録（ps / exec 用）
        ├── cgroup/cgroup.go        # cgroup v2 の作成・上限設定・後片付け
        ├── image/
        │   ├── registry.go         # Docker Registry API v2（token / manifest / layer）
        │   └── extract.go          # tar.gz の展開・whiteout の処理
        ├── overlay/overlay.go      # OverlayFS のマウント
        └── network/network.go      # Could。veth / bridge / NAT
```

**作らないもの**: `00_ideation.md` / `01_validation.md` / `02_mvp_spec.md` / `04_code_review.md` / `05_monetization.md` / `meta.yaml` / VitePress・Next.js のビルド構成 / フロントエンド（これは CLI なので画面はない）。

---

## 4. 実装の設計方針（ここが教材の心臓部）

### 4.1 なぜ「自分を再実行する」のか（re-exec パターン）

**この教材でいちばん最初にぶつかる、いちばん説明が要る箇所。**

素朴に考えると「`unshare` して、そのあと目的のコマンドを `exec` すればいい」と思える。ところが:

- ⚠ **PID namespace は、`unshare` を呼んだプロセス自身には適用されない。** 適用されるのは**その後に作られた子プロセス**から。「自分が PID 1 になる」ことはできない
- ⚠ **Go のランタイムはマルチスレッド**なので、プロセス内で `unshare(CLONE_NEWNS)` のようなスレッド単位の操作を素直に扱えない

そこで採る定石が **re-exec パターン**:

```
mydocker run alpine /bin/sh
   │
   ├─ 親: exec.Command("/proc/self/exe", "init", ...) を
   │       Cloneflags 付きで起動する ← ここで namespace が生まれる
   │
   └─ 子（新しい namespace の中・PID 1）: "init" サブコマンドとして起動され、
          pivot_root → /proc マウント → 目的のコマンドを syscall.Exec で置き換える
```

`/proc/self/exe` は**自分自身の実行ファイル**を指す。つまり「自分をもう一度、別の顔（`init` サブコマンド）で起動する」。
**この `init` サブコマンドは利用者が直接叩くものではない**ので、`--help` には出さず、README に「内部用」と明記する。

Go での書き方（この形になる）:

```go
cmd := exec.Command("/proc/self/exe", append([]string{"init"}, args...)...)
cmd.SysProcAttr = &syscall.SysProcAttr{
	Cloneflags: syscall.CLONE_NEWUTS | // ホスト名
		syscall.CLONE_NEWPID | // プロセス番号
		syscall.CLONE_NEWNS | // マウント
		syscall.CLONE_NEWIPC, // プロセス間通信
	Unshareflags: syscall.CLONE_NEWNS, // ★マウントの伝播をホストから切る
}
```

⚠ **`Unshareflags: syscall.CLONE_NEWNS` を忘れない。** これが無いと、コンテナ内で行ったマウントが
**ホスト側にも伝播する**（マウントの共有伝播）。`pivot_root` が `EINVAL` で落ちる原因にもなる。
「なぜか動かない」の筆頭なので、教材では落とし穴ボックスで先回りすること。

### 4.2 pivot_root（根の挿げ替え）

`chroot` ではなく `pivot_root` を使う。理由は教材で説明すること（`chroot` は抜け出す既知の手口があり、
古い root のマウントも残る。`pivot_root` は**古い root を完全に切り離せる**）。

手順は決まっている。**この順序を崩すと必ず失敗する:**

1. `mount("", "/", "", MS_PRIVATE|MS_REC, "")` — マウントの伝播を切る
2. `mount(rootfs, rootfs, "", MS_BIND|MS_REC, "")` — ⚠ **新しい root は「マウントポイント」でなければならない**。自分自身に bind mount して条件を満たす
3. `mkdir rootfs/.old_root`
4. `syscall.PivotRoot(rootfs, rootfs+"/.old_root")`
5. `syscall.Chdir("/")`
6. `mount("proc", "/proc", "proc", 0, "")` — ⚠ **これをやらないと `ps` がホストのプロセスを表示する**
7. `syscall.Unmount("/.old_root", MNT_DETACH)` と `os.Remove("/.old_root")` — 古い root を捨てる

⚠ **6 を忘れると PID namespace が効いていないように見える。** 実際には効いているのに、`/proc` が
ホストのものを指したままなので `ps` がホストの全プロセスを出す。**「namespace が壊れている」と誤診する典型**。
教材ではこれを「実際に失敗させてから」書くこと。

`/sys` と `/dev` も必要に応じてマウントする（`ps` が動く最低限は `/proc` だけでよい）。

### 4.3 cgroup v2 で資源に上限をかける

cgroup v2 はファイルシステム操作そのもの。`/sys/fs/cgroup/` 配下にディレクトリを作り、ファイルに値を書く。

```
/sys/fs/cgroup/mydocker/<コンテナID>/
├── memory.max      ← "10485760"（バイト）
├── cpu.max         ← "50000 100000"（100ms のうち 50ms = 0.5 コア）
├── pids.max        ← "64"
└── cgroup.procs    ← ここに PID を書くとそのプロセスが所属する
```

⚠ **親の `cgroup.subtree_control` でコントローラを有効にしないと、子で `memory.max` が作られない。**

```sh
echo "+memory +cpu +pids" > /sys/fs/cgroup/mydocker/cgroup.subtree_control
```

⚠ **cgroup v2 には「内部プロセス禁止」の規則がある。** コントローラを有効にしたディレクトリの
`cgroup.procs` には直接プロセスを入れられない（葉のディレクトリにだけ入れる）。階層設計をここで誤ると
`EBUSY` や `ENOTSUP` が出る。

⚠ **上限を設定しただけで満足しない。** 「本当に効いているか」は**実際に超過させて確かめる**:

```sh
# メモリ上限 10MB のコンテナで 100MB 確保しようとする → OOM で殺されるはず
mydocker run --memory 10m alpine sh -c 'dd if=/dev/zero of=/dev/null bs=100M count=1'
```

これを **E2E テストに入れる**（§8）。「設定ファイルに書けた」ことと「効いている」ことは別。

**後片付け**: コンテナ終了時に cgroup ディレクトリを `rmdir` する。⚠ 中にプロセスが残っていると消せない。

### 4.4 イメージの取得（Docker Registry HTTP API v2）

`docker pull` を呼ばず、自分で HTTP を叩く。**3段構え**である:

**① トークンを取る**（Docker Hub は匿名でもトークンが要る）

```
GET https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/alpine:pull
→ {"token": "..."}
```

**② マニフェストを取る**（以降 `Authorization: Bearer <token>`）

```
GET https://registry-1.docker.io/v2/library/alpine/manifests/latest
Accept: application/vnd.docker.distribution.manifest.v2+json,
        application/vnd.docker.distribution.manifest.list.v2+json,
        application/vnd.oci.image.manifest.v1+json,
        application/vnd.oci.image.index.v1+json
```

⚠ **`Accept` ヘッダを付けないと古い形式（schema 1）が返ってくる。** 必ず付ける。

⚠ **返ってくるのが「マニフェストリスト（インデックス）」のことがある。** これは複数 CPU アーキテクチャの
入り口なので、**`amd64` / `linux` の項目を選んで、その digest でもう一度取り直す**必要がある。
「取れた JSON に `layers` が無い」と悩む典型。教材の落とし穴に入れること。

**③ レイヤを落として展開する**

```
GET https://registry-1.docker.io/v2/library/alpine/blobs/<layer の digest>
```

各レイヤは **tar.gz**。`compress/gzip` + `archive/tar` で展開する。

⚠ **whiteout ファイルを処理する。** 上の層で「消したファイル」は `.wh.<名前>` という名前の
特殊なエントリとして入っている（ディレクトリ丸ごとの場合は `.wh..wh..opq`）。
これを普通のファイルとして展開すると、**消えるべきファイルが残り、変な名前のファイルが増える**。

⚠ **tar の中の絶対パス・`..` を弾く。** 展開先の外にファイルを書けてしまう（Zip Slip）。
展開先を基準に正規化し、外に出るパスは拒否すること。**セキュリティの話として教材に書く価値がある。**

⚠ Docker Hub には匿名アクセスの**取得回数制限**がある。開発中に何度も引くと止まる。
**一度落としたレイヤはローカルにキャッシュする**（digest をファイル名にすれば自然にキャッシュになる）。

### 4.5 OverlayFS（層を重ねる）

```
mount -t overlay overlay -o lowerdir=L3:L2:L1,upperdir=U,workdir=W /merged
```

- `lowerdir` が読み取り専用の下層、`upperdir` が書き込み先、`merged` が見える結果
- ⚠ **`lowerdir` は左が上（優先）。** イメージのレイヤは「後のものほど上」なので、
  **並べる順を逆にする**必要がある。ここを間違えると、古い層が新しい層を隠して**更新が反映されていないように見える**
- ⚠ **`workdir` は `upperdir` と同じファイルシステム上になければならない**。別だと `EXDEV` / `EINVAL` で落ちる
- ⚠ `workdir` は空でなければならない

コンテナを消すときは `upperdir` を捨てるだけでよい——**これがコンテナの「使い捨て」の正体**。
教材ではここを気持ちよく書くこと。

### 4.6 ネットワーク（Could。手を出すのは最後）

やることは4つ。**どれか1つ欠けても通信できず、原因の切り分けが難しい**:

1. bridge を作る（`mydocker0`）
2. veth ペアを作り、片方をホストの bridge に、もう片方をコンテナの netns に入れる
3. コンテナ側で IP を振り、デフォルトルートを bridge に向ける
4. ホストで **IP フォワード有効化** と **NAT（MASQUERADE）** を設定する

⚠ 4 を忘れると「コンテナから ping は出るのに応答が返らない」。⚠ `/etc/resolv.conf` を用意しないと
名前解決だけ失敗する（IP 直打ちなら通るのに `curl example.com` が失敗する）。

**Must と Should が終わるまで着手しない。** 時間が無ければ 09 章で「なぜ難しいか」を書いて終わりにしてよい。

---

## 5. 要件定義書の型（`mydocker-requirements.md`）

### 冒頭（この形で書く）

```markdown
# 自作コンテナランタイム mydocker 要件定義書（学習用）

> この文書は「**何を・なぜ作るか**」を定義する。
> アーキテクチャ・システムコールの選択・API設計などの「**どう作るか**」は含めない（それらは設計フェーズで行う）。
```

### 章立て（7章。章間は `---` で区切る）

1. 概要（1.1 背景 / 1.2 目的）
2. 用語定義（2列テーブル。コンテナ / イメージ / レイヤ / 名前空間 / 資源制限 …）
3. スコープ（3.1 対象範囲（MVP）/ 3.2 スコープ外）
4. 前提・制約（`**形態**:` `**動作環境**:` `**技術領域**:` の太字ラベル箇条書き）
5. 機能要件（FR-1〜FR-n）
6. 非機能要件（2列テーブル）
7. 発展課題（スコープ外・将来）

§5 の冒頭に必ずこの1行を置く:

> 各要件は「ユーザーストーリー」と「受け入れ条件」で記述する。受け入れ条件を満たせばその要件は完了とみなす。

### FR-n の書き方

`### FR-n <短い名詞句>` → `**ストーリー**:` → `**受け入れ条件**:`。
**ストーリーは「〜として、〜したい。なぜなら〜だから。」の3節構造**で、必ず「なぜなら」で終える。

実例（この粒度・この文体で書く）:

```markdown
### FR-4 資源の上限
**ストーリー**: 利用者として、コンテナが使えるメモリと CPU に上限を設けたい。なぜなら1つのコンテナの暴走でホスト全体が巻き添えになるのを防ぎたいから。
**受け入れ条件**:
- [ ] メモリ上限を指定して起動できる。
- [ ] 上限を超えてメモリを確保しようとしたプロセスは、確保に失敗するか強制終了される。
- [ ] CPU の使用割合に上限を指定でき、上限を超えて使い続けられない。
- [ ] コンテナの終了後、設定した制限はホストに残らない。
```

**⚠ 「namespace を使う」「cgroup v2 に書く」は要件ではない。** それは実現手段なので書かない。
要件に書くのは「利用者に何が起きるか」だけ（上の例に `cgroup` の語が出てこないのが要点）。
ただし §4 前提・制約の `**技術領域**:` に、学習目的として
**「コンテナ関連ライブラリを使わず、カーネルの機能を直接扱うことを含む」**と1行書いてよい。

受け入れ条件のルール:

- **`- [ ]` の未チェックチェックボックス**で書く
- **1条件1文、必ず「〜できる。」「〜される。」で言い切る**。体言止めにしない
- **観測可能な振る舞いだけ書く**
- **異常系・境界を必ず1条は入れる**（「終了後に残らない」「存在しないイメージを指定するとエラーになる」）
- 条件数は2〜4個

### FR に必ず含めるもの

- コンテナの起動（`run`）と、指定したコマンドの実行
- **ホストから隔離された視界**（プロセス一覧・ホスト名・ファイルシステムがホストと別に見える）
- **資源の上限**（上の FR-4 の例）
- 実行中コンテナの一覧（`ps`）
- Should を実装するなら: イメージの取得 / 層の重ね合わせ / `exec`

### NFR の表

表の直前に `要件（達成すべき状態）を記す。実現手段は設計フェーズで決める。` の1行を置く。

| 分類 | 要件 |
|------|------|
| 安全性 | コンテナ内のプロセスが、ホストのファイルを意図せず読み書きできない。 |
| 安全性 | 取得したイメージの中身が、展開先として指定した場所の外にファイルを作れない。 |
| 信頼性 | コンテナが異常終了しても、ホストに設定や一時ファイルが残らない。 |
| 可搬性 | 同じ手順で、開発者以外の環境でも同じ結果が得られる。 |
| 保守性・学習性 | 各機能を独立して確認・テストできる単位で開発できる。 |
| 保守性・学習性 | 主要な設計判断は、選択肢と理由を記録として残せる状態にする。 |

**手段語を排して状態で書く。**

### スコープ外・発展課題

§3.2 は体言止めの箇条書きで列挙し、**末尾に橋渡しの1文**を置く:

> スコープ外の項目は「不要」ではなく、MVP完成後に明確な目的をもって追加する発展課題とする（§7）。

§7 は §3.2 を受けて**太字の見出し語＋補足説明**で再掲する。文末に `---` の後、斜体1行:

> *この要件定義はMVPの合意事項を定める。設計・実装はここを起点に進める。*

**スコープ外に入れるもの**: イメージのビルド（Dockerfile 相当）／レジストリへの push／複数コンテナのオーケストレーション／
ボリューム管理／`docker compose` 相当／Windows・macOS でのネイティブ動作／rootless 実行／seccomp・AppArmor。

---

## 6. 実装の進め方（Phase A〜F）

### Phase A: 疎通実験（§1）

`00_smoke_test.md` に**実際の出力**を貼る。ここでスコープを確定させる。

### Phase B: 要件定義

`mydocker-requirements.md` を §5 の型で書く。**疎通実験の結果を反映した**スコープにする。

### Phase C: 実装

この順で作る。**下から積む。1段飛ばすと切り分けが地獄になる。**

1. `src/devcontainer/docker-compose.yml`（特権 Linux 開発環境）と `go.mod`
2. **`internal/container/run.go` + `init.go`** — re-exec パターンで namespace を作り、`/bin/sh` が動くところまで。この時点では rootfs は**ホストのものをそのまま使ってよい**
3. **`pivot_root` と `/proc` の再マウント** — ここで初めて「別の世界」になる。`ps` が自分だけを表示することを確認
4. **`internal/cgroup`** — メモリ上限が**実際に効く**ことを、超過させて確認
5. `internal/container/state.go`（`ps` 用の記録）
6. Should: `internal/image`（Registry API）→ `internal/overlay`
7. Could: `internal/network`
8. `src/README.md`

⚠ **2 と 3 の間で必ず一度動かす。** namespace だけの状態と pivot_root 後の状態は
見え方が全く違うので、順に確認しないと「どちらが原因か」が分からなくなる。

### Phase D: E2E（§8。省略禁止）

### Phase E: 自己レビュー → 修正 → 再テスト（§7。ここが最重要）

### Phase F: ドキュメント（`03_implementation.md` と `CLAUDE.md`。§9 の型で）

### Phase G: 教材サイト（§10・§11・§12 の型で。時間が残っていれば）

時間が尽きたら、`docs/_引き継ぎ指示.md` の進捗チェックリストに**どこまで書いたかを正確に**残して止まる。

---

## 7. 自己レビューで必ず検査すること

実装が通ったら、**自分の実装を疑ってレビューする**。以下は繰り返し踏まれる失敗の型なので、
**1つずつ明示的に確認し、確認した方法を `03_implementation.md` に書く**こと。

### 一般

- **dead code の検査** — 「実装済み・テスト通過」でも CLI から呼ばれていない機能は要件未達。**コマンドの入口から末端まで経路を辿って**確認する
- **「多すぎ」側の assert** — 件数の検査を「0件でない」だけで済ませない。`ps` の出力は**期待する件数ちょうど**かを見る。冪等な操作は2回呼ぶ
- **テストの空振り** — 対象0件でも失敗0で緑になる。**検証した件数を実数で出力**する
- **修正の適用確認** — 「修正した」で終わらせず、**現物を grep して**適用されていることを確かめる

### この教材に固有（ここが本番）

- **「隔離できている」を思い込みで判定しない。** 次を**実際に実行して出力を見る**:
  - コンテナ内の `ps` にホストのプロセスが**1つも出ない**こと（`/proc` 再マウント漏れの検出）
  - コンテナ内の `hostname` がホストと**違う**こと
  - コンテナ内から `ls /` がイメージの中身であり、**ホストの `/home` などが見えない**こと
- **cgroup が「書けた」ではなく「効いた」ことを確かめる** — 上限を超えるメモリ確保が**実際に失敗する**ことを見る。`cat memory.max` が期待値を返すだけでは不十分
- **後片付けの検査** — コンテナ終了後に次が**残っていない**ことを確認する:
  - `/sys/fs/cgroup/mydocker/` 配下のディレクトリ
  - overlay のマウント（`mount | grep overlay` が増え続けていないか）
  - ⚠ **マウントのリークは静かに進行する**。10回起動して10個残っていないか、**数えて**確かめる
- **アンマウント失敗時に削除へ進んでいないか** — ⚠ `umount` は `EBUSY` で**日常的に失敗する**。
  コンテナ削除の実装が「アンマウント → ディレクトリ削除」の順で、**前者のエラーを無視している**と、
  マウント越しに `lowerdir`（＝イメージの実体）まで消える。**`umount` のエラーを必ず握り、
  失敗したら削除に進まない**こと。実際に使用中の状態を作って（コンテナ内に居座らせて）削除を試し、
  **イメージが無事であることを確認する**
- **ホストを壊していないか** — `Unshareflags` 漏れでマウントがホストに伝播していないか。**ホスト側で `mount | wc -l` を実行前後で比較する**
- **tar 展開のパス検証** — `../` や絶対パスを含む tar を**自分で作って**投入し、展開先の外にファイルが作られないことを確認する。「弾いているはず」で済ませない
- **エラー時に中途半端な状態を残さないか** — 起動の途中で失敗させて（存在しないコマンドを指定するなど）、cgroup とマウントが片付くことを見る

---

## 8. E2E テストの型（`src/e2e_test.py`）

**Python 標準ライブラリのみで書く。** これは CLI なので `subprocess` でコマンドを叩く。
**特権 Linux 環境の中で実行する**（`docker compose run --rm dev python3 e2e_test.py`）。

### ハーネス（この形で書く）

```python
# -*- coding: utf-8 -*-
"""自作コンテナランタイム mydocker E2E テスト（要件の受け入れ基準に沿う）"""
import subprocess
import sys
import time

BIN = "./mydocker"

ok_count = 0
fail_count = 0


def check(name, cond, detail=""):
    global ok_count, fail_count
    if cond:
        ok_count += 1
        print(f"  OK   {name}")
    else:
        fail_count += 1
        print(f"  FAIL {name} {detail}")


def run(args, timeout=30):
    """mydocker を起動し (returncode, stdout, stderr) を返す。例外にしない。"""
    try:
        p = subprocess.run([BIN] + args, capture_output=True, text=True, timeout=timeout)
        return p.returncode, p.stdout, p.stderr
    except subprocess.TimeoutExpired:
        return -1, "", "TIMEOUT"


def sh(cmd, timeout=30):
    """ホスト側のコマンド（検査用）。"""
    p = subprocess.run(cmd, shell=True, capture_output=True, text=True, timeout=timeout)
    return p.returncode, p.stdout, p.stderr
```

末尾:

```python
print()
print(f"RESULT: {ok_count} passed, {fail_count} failed")
sys.exit(1 if fail_count else 0)
```

### 並べ方

- **セクション見出しに要件IDを埋める** — `print("== 3. 資源の上限 (FR-4) ==")`
- **check 名は日本語の平叙文**でユーザー視点の振る舞いを書く
- **正常系の直後に異常系をペアで置く**
- **状態の後始末を検査するセクションを最後に置く**（他の項目を壊すため）

### 必ず機械的に検証する項目

**隔離**（Must）:

1. コンテナ内の `ps` に**ホストのプロセスが出ない**（出力行数が期待どおり少ないことを**数える**）
2. コンテナ内の `hostname` がホストと違う
3. コンテナ内から**ホストのファイルが見えない**（`ls /` の中身がイメージのもの）
4. コンテナ内のプロセスが **PID 1** である

**資源制限**（Must）:

5. **メモリ上限を超える確保が実際に失敗する**（`cat memory.max` だけで通さない）
6. コンテナ終了後、`/sys/fs/cgroup/mydocker/` 配下に**ディレクトリが残っていない**

**後片付け・リーク**（Must。ここを落とすと静かに壊れる）:

7. **10回連続で起動・終了しても、overlay のマウント数が増え続けない**（実行前後で数えて比較）
8. **起動に失敗したとき（存在しないコマンド指定）も、cgroup とマウントが残らない**

**Should を実装したなら**:

9. イメージを取得して起動でき、2回目はキャッシュから起動する
10. `ps` に実行中のコンテナが**ちょうど期待件数**表示される
11. 上の層で削除されたファイルが、重ね合わせ後に**見えない**（whiteout の検証）
12. **`../` を含む細工した tar が、展開先の外にファイルを作れない**

---

## 9. `CLAUDE.md` の型（7セクション）

タイトルは `# CLAUDE.md — 自作コンテナランタイム mydocker（学習用）+ 教材サイト`。

**① プロジェクト概要** — 1段落で「何を学ぶプロジェクトか」＋「市場検証スキップ」。成果物を**ファイルパスとその役割**で1行1件。

**② 技術スタック・構成** — 3列表 `| 層 | 技術 | 場所 |`。**動作環境（Linux 前提・特権が要ること）を必ず書く**。

**③ 動かし方** — 3行程度のコマンドブロック。**各行に行末コメントで「何が起きるか＋なぜ」**。
⚠ **「Windows / macOS では動かない。開発環境の入り方」を最初に書く**（これが分からないと誰も動かせない）。

**④ 主要な設計判断（詳細は 03_implementation.md）** — `- **関心事**: ` の形。
**採用した方式 →（正式名称）→ なぜそうしたか／なぜ他を退けたか**まで1〜2文。
`chroot` でなく `pivot_root`、`docker pull` でなく Registry API 直叩き、などをここに。

**⑤ 落とし穴（このプロジェクト固有）** — **実際に踏んだものだけ**。一般論は書かない。
**症状 →（原因）→ `→` で対処**の順。⚠ **疎通実験で分かった環境固有の事情（cgroup のバージョン等）は必ずここに書く。**

**⑥ 教材サイトの編集方針（docs/ 版）** — 章構成 / 章↔ソースファイルの対応 / **インラインCSS禁止** /
アクセント色 / **教材に出す数値・コマンド出力は実物と整合させる** / **掲載コードを変えたら該当章も同期（一字一句一致）**。

**⑦ 収益化モデル** — `なし（学習目的）。成果は技術記事・note の素材として転用する。`

### `03_implementation.md`

設計判断を**「判断軸 / 対抗案 / 結果」の形式で番号付き列挙**。踏んだバグと教訓も書く。
⚠ **この教材は「動かない時間」が長い**ので、**何にどれだけ詰まったか**を書いておくと記事の骨になる。

---

## 10. 教材サイトの方針

### 読者像（これを外すと全部ずれる）

**読者は「初心者に毛が生えた程度」の一人。** プログラミングの基礎文法とターミナル操作はできるが、
**Linux カーネルの機能（namespace / cgroup / マウント）の予備知識はほぼゼロ**。
`docker run` は使ったことがあるが、中で何が起きているかは知らない——そういう一人。

**この一人が、初級から始めて最後には高度な部分まで「暗記ではなく理解」できること**が唯一の成功条件。
**つまずいたら読者のせいではなく、階段の一段が高すぎた教材のせい。段差を見つけたら段を増やす。**

### 初級→高度へ「無理なく登る」ための11の技法（各章で当てはまるものは必ず使う）

1. **ゴールを先に触らせる** — 序章で完成品を5分動かす。山頂を見てから登ると迷わない
2. **前提を最小限に宣言し、必要な分だけ初出で補う** — 「必要なのは Go の基礎文法とターミナルだけ」と明言。カーネル特有の用語は**序章に「道具箱」節でまとめ**、各章の初出では `GO メモ` / `LINUX メモ` の補足ボックスでその場で補う。「あとで説明します」を乱発しない
3. **素朴案の破綻から必然を導く** — いきなり正解を出さない。「`chroot` すれば隔離できる？ → プロセス一覧はホストのまま見える → だから PID namespace」のように、**素朴な方法が壊れる様を見せてから**本命を出す
4. **数字で体感させる** — 「仮想マシンは起動に数十秒・数百MB、コンテナは数十ミリ秒」「alpine のイメージは約 3〜4MB、レイヤは1枚」のように**具体的な数値**で掴ませる。⚠ **数値は必ず自分の環境で実測して書く**（`docker images` の実表示など）
5. **内部状態を図解する（ASCII図）** — namespace の入れ子、pivot_root の前後、overlay の層構造は**必ず ASCII 図**にする
6. **1段 = 1ステップに割る** — 各章冒頭に「この章のゴール」と「ステップ一覧」を置く
7. **つまずきを落とし穴ボックスで先回り** — §4 に挙げた地雷（`Unshareflags` 漏れ、`/proc` 再マウント漏れ、`lowerdir` の順序、マニフェストリスト、whiteout）を `⚠ 落とし穴` で**先に**警告する
8. **各ステップに「動かして確認」の関所** — **実際に打つコマンドと出るはずの出力**を載せる。この教材では「`ps` の出力がこう変わる」が最高の教材になる
9. **コードは断片でなく全文、`src/` の実装と一字一句一致** — 省略は `// …（後のステップで）` と**明示**する
10. **層を下から積む＝飛ばせない構成を明言** — 「1章飛ばすと次章は必ず分からない」ことを序章で告げる
11. **章末に FAQ と理解度チェック** — 読了目安時間も添える

### トーン

- **一人称の対話調**。「〜します」「〜してください」で、読者の隣に座って一緒に手を動かす語り口
- **概念の初出で用語を定義**し、以降は断りなく使う。章の途中で用語表を一度まとめる
- **「なぜそう設計したか」を必ず書く** — 他の選択肢を退けた理由（トレードオフ）を残す。これが「暗記ではなく理解」の核

### 章立て（序章 + 第00〜09章 = 全11ページ）

`docs/_引き継ぎ指示.md` にこの表を書き、進捗を管理する。

| nav-num | ラベル | nav-tag | href | 担当 src ファイル |
|---|---|---|---|---|
| 序 | はじめに・準備 | Setup | index.html | — |
| 00 | Linux の中に入る（開発環境） | Env | 00-env.html | devcontainer/docker-compose.yml, go.mod |
| 01 | 自分をもう一度起動する（re-exec） | Reexec | 01-reexec.html | container/run.go, container/init.go |
| 02 | 見える世界を区切る（namespace） | Namespace | 02-namespace.html | container/run.go（Cloneflags）, container/init.go（hostname） |
| 03 | 根を挿げ替える（pivot_root） | Root | 03-pivotroot.html | container/init.go（pivot_root・/proc） |
| 04 | 資源に上限をかける（cgroup） | Cgroup | 04-cgroup.html | cgroup/cgroup.go |
| 05 | イメージを取ってくる（Registry API） | Registry | 05-registry.html | image/registry.go |
| 06 | 層を重ねる（OverlayFS） | Layer | 06-overlay.html | image/extract.go, overlay/overlay.go |
| 07 | 道具にする（CLI と状態管理） | CLI | 07-cli.html | cli/cli.go, container/state.go, cmd/mydocker/main.go |
| 08 | 外とつながる（ネットワーク） | Network | 08-network.html | network/network.go（未実装なら「なぜ難しいか」の章にする） |
| 09 | さらに先へ（発展・総括） | Beyond | 09-beyond.html | 発展課題＋全体総括 |

⚠ **実装できなかったものの章を「できたふり」で書かない。** ネットワークを実装しなかったなら、
08章は「**なぜ着手しなかったか・何が必要になるか・どこが難所か**」を書く章にする。それでも教材として成立する。

### 各章の主題

- **序 はじめに・準備**: 完成品を5分動かす（`mydocker run alpine /bin/sh` して `ps` を打つ）。**「コンテナは軽量な仮想マシンではない」**という中心命題を最初に置く。道具箱（namespace / cgroup / マウント / rootfs / レイヤ / レジストリの用語）。層が下から積まれ飛ばせない構成であることの宣言
- **00 Linux の中に入る（開発環境）**: **この教材は Windows でも macOS でも動かない**ことを正直に告げ、特権 Linux コンテナで開発環境を作る。「Docker を作るために Docker を使う」入れ子を笑いながら説明する。⚠ 疎通実験（§1）をそのまま読者の作業にする——**環境によって cgroup のバージョンが違い、できることが変わる**
- **01 自分をもう一度起動する（re-exec）**: **最初の山場**。「`unshare` して `exec` すればいい」という素朴案が、**PID namespace は自分には効かない**という一点で破綻する。だから `/proc/self/exe` で自分を別の顔で起動する。⚠ `Unshareflags` を忘れるとマウントがホストに漏れる
- **02 見える世界を区切る（namespace）**: 6種類の namespace を1つずつ。**UTS（ホスト名）から始める**のが分かりやすい（`hostname` を打つだけで違いが見える）。次に PID。⚠ **この時点ではまだ `ps` がホストを見せる**——その謎を03章に引っ張る
- **03 根を挿げ替える（pivot_root）**: **02章の謎が解ける章**。`/proc` がホストのままだったから `ps` がホストを見せていた。`chroot` を先に試して**抜け出せてしまう**ことを見せ、`pivot_root` の必然を導く。手順の順序（伝播を切る → 自分に bind mount → pivot → `/proc` → 古い root を捨てる）を1段ずつ
- **04 資源に上限をかける（cgroup）**: cgroup v2 は**ただのファイル操作**だと分かると急に easy になる。⚠ `cgroup.subtree_control`／「内部プロセス禁止」の規則。**上限を超えさせて実際に殺されるところを見せる**のがこの章の関所
- **05 イメージを取ってくる（Registry API）**: `docker pull` の中身。トークン → マニフェスト → レイヤの3段。⚠ **`Accept` ヘッダ**／**マニフェストリストで一段深い**／**取得回数制限**。「イメージとは tar.gz の列と JSON である」という身も蓋もない事実に着地させる
- **06 層を重ねる（OverlayFS）**: **もうひとつの山場**。⚠ `lowerdir` は**左が上**／`workdir` の制約／**whiteout**。「コンテナを消す＝`upperdir` を捨てるだけ」で使い捨ての正体に着地
- **07 道具にする（CLI と状態管理）**: `run` 以外のコマンド。実行中コンテナをどこに記録するか。⚠ **後片付け**（cgroup とマウントのリーク）をここで正面から扱う
- **08 外とつながる（ネットワーク）**: veth / bridge / NAT。実装したならコードで、しなかったなら**難所の地図**として書く。⚠ NAT 忘れ／`resolv.conf` 忘れ
- **09 さらに先へ（発展・総括）**: **自作したものに何が足りないか**を正直に列挙する（OCI ランタイム仕様との差、`runc` が実際にやっていること、seccomp / capabilities による権限の絞り込み、rootless、イメージのビルド）。全体を総括

**⚠ 数値とコマンド出力は必ず実測して書く。** イメージのサイズ、起動時間、`ps` の出力は**自分の環境で実際に取ったもの**を貼る。憶測で書かない。

### 執筆の進め方

1. **まず序章 `index.html` と `00-env.html` を1つの subagent に書かせる**
2. その `aside.spine`（全ページ共通の目次）を**見本としてコピー**させ、01〜09 を章ごとに1 subagent で並列展開
3. 各 subagent には担当 src ファイルを Read させ、**コードを一字一句一致で掲載**させる（省略は `// …` を明示）
4. 「動かして確認」には**実在するコマンドと実際の出力**を使う
5. **分量は1章 57KB 以上**

### 完成後の品質チェック（機械的に実行する）

- `aside.spine` が全11ページで完全一致しているか（`current` の位置だけが違う）
- `pager`（前へ／次へ）が全章で正しく連結しているか。**リンク切れがないか**
- 掲載コードの関数名・型名が `src/` に**実在するか**を照合する
- `go build ./... && go vet ./...` が通るか
- `docs/` 内にインライン `style=` が残っていないか（**インラインCSS禁止**）
- **教材に載せたコマンド出力が、実際の出力と一致しているか**（特に `ps` の出力）

---

## 11. `docs/style.css`（全文。これをそのままコピーして使う）

**アクセント色だけ変えること。** コンテナ／低レイヤの教材なので、`--accent` は青系を推奨
（例: `--accent:#2a6f97; --accent-ink:#1d5578; --accent-soft:rgba(42,111,151,.12);` と `pre .kw{color:#8ab4d4}`）。
それ以外の値は触らない。

```css
/* コンテナをつくる — 共通スタイル */
:root{
  --ink:#212a2e;
  --substrate:#141b20;
  --substrate-2:#1c252b;
  --paper:#f3f4f0;
  --panel:#fbfbf8;
  --accent:#1e7a6b;
  --accent-ink:#155a4f;
  --accent-soft:rgba(30,122,107,.12);
  --warn:#a9512b;           /* hazard 専用 */
  --warn-soft:rgba(169,81,43,.10);
  --muted:#6b7680;
  --rule:#dcded6;
  --rule-dark:#2c363d;
  --code-bg:#f0f1ec;
  --serif:"Source Serif 4","Noto Serif JP",Georgia,"Yu Mincho",serif;
  --mono:"IBM Plex Mono",ui-monospace,"SFMono-Regular",Menlo,monospace;
  --measure:70ch;
}

*{box-sizing:border-box}
html{scroll-behavior:smooth}
body{
  margin:0;
  background:var(--paper);
  color:var(--ink);
  font-family:var(--serif);
  font-size:18px;
  line-height:1.9;
  -webkit-font-smoothing:antialiased;
  text-rendering:optimizeLegibility;
}

.shell{display:grid;grid-template-columns:300px 1fr;min-height:100vh}

/* ---------- spine (sidebar) ---------- */
.spine{
  background:var(--substrate);
  color:#cfd6d3;
  padding:38px 26px 30px;
  position:sticky;top:0;height:100vh;overflow-y:auto;
  border-right:1px solid var(--rule-dark);
}
.wordmark{font-family:var(--mono);letter-spacing:.22em;font-weight:600;
  font-size:.72rem;color:var(--accent);text-transform:uppercase;text-decoration:none;display:block}
.book-title{font-family:var(--serif);font-weight:700;font-size:1.5rem;
  line-height:1.35;color:#f4f5f2;margin:.5rem 0 .2rem}
.book-sub{font-family:var(--mono);font-size:.66rem;letter-spacing:.14em;
  text-transform:uppercase;color:#7f8b8a;margin-bottom:26px}

.legend{font-family:var(--mono);font-size:.6rem;letter-spacing:.1em;
  color:#6f7b7a;text-transform:uppercase;display:flex;gap:14px;
  padding:0 0 16px 2px;border-bottom:1px solid var(--rule-dark);margin-bottom:14px}
.legend b{color:var(--accent);font-weight:600}

.toc{list-style:none;margin:0;padding:0;position:relative}
.toc::before{content:"";position:absolute;left:14px;top:12px;bottom:12px;
  width:2px;background:var(--rule-dark)}
.nav-item{position:relative;display:grid;grid-template-columns:28px 1fr;
  align-items:baseline;gap:12px;width:100%;text-align:left;cursor:pointer;
  background:none;border:0;color:#aeb7b5;font-family:var(--serif);
  padding:9px 8px 9px 0;border-radius:7px;transition:background .18s,color .18s;
  text-decoration:none}
.nav-num{font-family:var(--mono);font-size:.66rem;letter-spacing:.06em;
  color:#7f8b8a;text-align:right;position:relative}
.nav-num::after{content:"";position:absolute;left:-1px;top:.35em;width:9px;height:9px;
  transform:translateX(-50%);border-radius:50%;background:var(--substrate);
  border:2px solid #47555c;transition:background .18s,border-color .18s,box-shadow .18s}
.nav-label{line-height:1.35}
.nav-tag{display:block;font-family:var(--mono);font-size:.58rem;letter-spacing:.12em;
  text-transform:uppercase;color:#63706f;margin-top:1px}
.nav-item:hover{background:#20292f;color:#e4e9e7}
.nav-item:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
.nav-item.current{color:#f4f5f2}
.nav-item.current .nav-num{color:var(--accent)}
.nav-item.current .nav-num::after{background:var(--accent);border-color:var(--accent);
  box-shadow:0 0 0 4px var(--accent-soft)}
.nav-item.current .nav-tag{color:var(--accent)}

.spine-foot{font-family:var(--mono);font-size:.58rem;letter-spacing:.08em;
  color:#5c6968;margin-top:26px;padding-top:16px;border-top:1px solid var(--rule-dark);
  line-height:1.7}

/* ---------- reading column ---------- */
.reading{padding:76px 8vw 120px;max-width:calc(var(--measure) + 16vw)}

.eyebrow{font-family:var(--mono);font-size:.7rem;letter-spacing:.2em;
  text-transform:uppercase;color:var(--accent-ink);margin-bottom:18px;
  display:flex;align-items:center;gap:12px}
.eyebrow .depth{color:var(--muted)}
.eyebrow::before{content:"";width:34px;height:2px;background:var(--accent)}

h1{font-family:var(--serif);font-weight:700;font-size:2.5rem;line-height:1.22;
  letter-spacing:-.01em;margin:0 0 .5rem}
h2{font-family:var(--serif);font-weight:700;font-size:1.5rem;line-height:1.35;
  margin:2.6rem 0 .8rem;padding-top:.4rem}
h3{font-family:var(--mono);font-weight:600;font-size:.82rem;letter-spacing:.1em;
  text-transform:uppercase;color:var(--accent-ink);margin:2rem 0 .4rem}
.lede{font-size:1.28rem;line-height:1.72;color:#2c363b;margin:.4rem 0 2rem;
  max-width:var(--measure)}
p{max-width:var(--measure);margin:0 0 1.25rem}
ul,ol{max-width:var(--measure)}
li{margin-bottom:.4rem}
a{color:var(--accent-ink);text-decoration-thickness:1px;text-underline-offset:3px}
strong{font-weight:700}
em{font-style:normal;background:var(--accent-soft);padding:0 .25em;border-radius:3px}
code{font-family:var(--mono);font-size:.82em;background:var(--code-bg);
  padding:.1em .4em;border-radius:4px}

/* step heading */
h2.step{display:flex;align-items:center;gap:12px;border-top:1px solid var(--rule);
  padding-top:1.6rem;margin-top:3rem}
.step-no{font-family:var(--mono);font-weight:600;font-size:.68rem;letter-spacing:.14em;
  text-transform:uppercase;color:#fff;background:var(--accent);
  padding:4px 10px;border-radius:999px;white-space:nowrap;flex:0 0 auto}

/* code block */
pre{font-family:var(--mono);font-size:.8rem;line-height:1.7;background:var(--substrate);
  color:#d7ded9;padding:20px 22px;border-radius:10px;overflow-x:auto;margin:1.4rem 0;
  max-width:var(--measure);border:1px solid var(--rule-dark);tab-size:4}
pre .cm{color:#7d8f89}      /* comment */
pre .kw{color:#7fb2a6}      /* keyword */
pre .ty{color:#c8b98a}      /* type */
pre .st{color:#b9c98f}      /* string */
pre code{background:none;padding:0;font-size:1em;color:inherit}

/* file tag: <div class="file-tag">internal/container/init.go</div> の直後に pre を置く */
.file-tag{font-family:var(--mono);font-size:.64rem;letter-spacing:.1em;
  color:#9fb0ab;background:var(--substrate-2);display:inline-block;
  padding:6px 14px;border-radius:8px 8px 0 0;margin:1.4rem 0 0;
  border:1px solid var(--rule-dark);border-bottom:0;position:relative;top:1px}
.file-tag + pre{margin-top:0;border-top-left-radius:0}

/* terminal block */
pre.term{background:#0d1418;border-color:#22303a}
pre.term .ps{color:var(--accent);font-weight:600}   /* プロンプト $ や # */
pre.term .out{color:#93a4a0}                        /* コマンドの出力 */

/* ascii diagram */
.ascii{font-family:var(--mono);font-size:.74rem;line-height:1.5;white-space:pre;
  background:var(--panel);border:1px solid var(--rule);color:#3a454b;
  padding:18px 20px;border-radius:10px;overflow-x:auto;margin:1.4rem 0;max-width:var(--measure)}

/* callouts */
.box{max-width:var(--measure);margin:1.6rem 0;padding:20px 22px 18px;
  border-radius:11px;background:var(--panel);border:1px solid var(--rule);
  position:relative}
.box-k{font-family:var(--mono);font-size:.64rem;letter-spacing:.16em;
  text-transform:uppercase;margin-bottom:.5rem;display:flex;align-items:center;gap:9px;
  color:var(--muted)}
.box p{margin-bottom:.6rem}
.box p:last-child{margin-bottom:0}
.box pre{margin-bottom:.2rem}
.box.goal{background:var(--accent-soft);border-color:rgba(30,122,107,.3)}
.box.goal .box-k{color:var(--accent-ink)}
.box.map{border-left:3px solid var(--accent)}
.box.map .box-k{color:var(--accent-ink)}
.box.do{border-left:3px solid var(--accent);background:var(--panel)}
.box.do .box-k{color:var(--accent-ink)}
.box.done{background:#eef3ee;border-color:#c9dcc9}
.box.done .box-k{color:#2f6b45}
.box.hazard{background:var(--warn-soft);border-color:rgba(169,81,43,.32)}
.box.hazard .box-k{color:var(--warn)}
.box ol,.box ul{margin:.2rem 0 0;padding-left:1.3em}
.box li{margin-bottom:.4rem}
.box li:last-child{margin-bottom:0}

/* GOメモ / LINUXメモ */
.gonote{max-width:var(--measure);margin:1.6rem 0;padding:18px 22px 16px;
  border-radius:11px;background:#f6f4ea;border:1px dashed #c9c2a1}
.gonote .box-k{color:#8a7a34}
.gonote p{margin-bottom:.6rem}
.gonote p:last-child{margin-bottom:0}

/* details: FAQ / 理解度チェック */
details.qa{max-width:var(--measure);margin:0 0 .8rem;border:1px solid var(--rule);
  border-radius:10px;background:var(--panel);overflow:hidden}
details.qa summary{cursor:pointer;padding:14px 18px;font-weight:600;list-style:none;
  position:relative;padding-right:44px}
details.qa summary::-webkit-details-marker{display:none}
details.qa summary::after{content:"+";position:absolute;right:18px;top:50%;
  transform:translateY(-50%);font-family:var(--mono);color:var(--accent);
  font-size:1.1rem;font-weight:600}
details.qa[open] summary::after{content:"−"}
details.qa summary:hover{background:var(--accent-soft)}
details.qa .qa-body{padding:4px 18px 14px;border-top:1px solid var(--rule)}
details.qa .qa-body p{margin-bottom:.6rem}

/* table */
table{border-collapse:collapse;max-width:var(--measure);margin:1.4rem 0;
  font-size:.92rem;width:100%}
th,td{border:1px solid var(--rule);padding:8px 14px;text-align:left;vertical-align:top}
th{background:var(--panel);font-family:var(--mono);font-size:.7rem;
  letter-spacing:.08em;text-transform:uppercase;color:var(--accent-ink)}
td code{white-space:nowrap}

blockquote{margin:2rem 0;padding:0 0 0 20px;border-left:3px solid var(--accent);
  max-width:var(--measure);font-size:1.12rem;color:#37424a}
blockquote cite{display:block;font-family:var(--mono);font-size:.66rem;letter-spacing:.1em;
  text-transform:uppercase;color:var(--muted);font-style:normal;margin-top:.6rem}

/* chapter footer nav */
.chap-foot{display:flex;justify-content:space-between;gap:16px;
  max-width:var(--measure);margin-top:3.5rem;padding-top:1.5rem;
  border-top:1px solid var(--rule)}
.pager{font-family:var(--mono);font-size:.72rem;letter-spacing:.05em;background:none;
  border:1px solid var(--rule);color:var(--ink);padding:11px 16px;border-radius:8px;
  cursor:pointer;transition:border-color .18s,background .18s;text-align:left;max-width:47%;
  text-decoration:none;display:block}
.pager:hover{border-color:var(--accent);background:var(--accent-soft)}
.pager:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
.pager .dir{display:block;color:var(--muted);font-size:.62rem;letter-spacing:.14em;
  text-transform:uppercase;margin-bottom:3px}
.pager.next{text-align:right;margin-left:auto}

.progress-tag{font-family:var(--mono);font-size:.62rem;letter-spacing:.12em;
  color:var(--muted);text-transform:uppercase;margin-bottom:34px}

.hero-rule{font-family:var(--mono);font-size:.68rem;letter-spacing:.2em;color:var(--muted);
  text-transform:uppercase;margin:2.2rem 0 0}

/* mobile */
@media (max-width:860px){
  .shell{grid-template-columns:1fr}
  .spine{position:static;height:auto;border-right:0;border-bottom:1px solid var(--rule-dark);
    padding:24px 18px}
  .toc::before{display:none}
  .toc{display:flex;gap:8px;overflow-x:auto;padding-bottom:6px;margin-top:6px}
  .nav-item{grid-template-columns:1fr;min-width:120px;background:#20292f;padding:10px 12px;
    flex:0 0 auto}
  .nav-num::after{display:none}
  .nav-num{text-align:left;margin-bottom:2px}
  .legend{display:none}
  .spine-foot{display:none}
  .reading{padding:44px 22px 90px}
  h1{font-size:1.95rem}
  .lede{font-size:1.14rem}
  body{font-size:17px}
}
```

**このサイトは JavaScript を一切使わない。** FAQ の開閉は `<details>` の標準機能で行う。

---

## 12. 章HTMLの骨格（この形で書く）

### `<head>`（全ページ共通・`<title>` だけ差し替え）

```html
<!DOCTYPE html>
<html lang="ja">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>第03章 根を挿げ替える — コンテナをつくる</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=IBM+Plex+Mono:wght@400;500;600&family=Source+Serif+4:opsz,wght@8..60,400;8..60,600;8..60,700&family=Noto+Serif+JP:wght@400;600;700&display=swap" rel="stylesheet">
<link rel="stylesheet" href="style.css">
</head>
<body>
<div class="shell">
```

### `<aside class="spine">`（全11ページで完全一致。`current` だけ移動）

```html
  <aside class="spine">
    <a class="wordmark" href="index.html">MYDOCKER · from scratch</a>
    <div class="book-title">コンテナを<br>つくる</div>
    <div class="book-sub">Go と Linux カーネルで Docker を再発明する</div>

    <div class="legend"><span>読む <b>↓</b></span><span>組む <b>↑</b></span></div>

    <nav aria-label="目次">
      <ul class="toc">
        <li><a class="nav-item" href="index.html"><span class="nav-num">序</span><span class="nav-label">はじめに・準備<span class="nav-tag">Setup</span></span></a></li>
        <li><a class="nav-item" href="00-env.html"><span class="nav-num">00</span><span class="nav-label">Linux の中に入る<span class="nav-tag">Env</span></span></a></li>
        <li><a class="nav-item" href="01-reexec.html"><span class="nav-num">01</span><span class="nav-label">自分をもう一度起動する<span class="nav-tag">Reexec</span></span></a></li>
        <li><a class="nav-item" href="02-namespace.html"><span class="nav-num">02</span><span class="nav-label">見える世界を区切る<span class="nav-tag">Namespace</span></span></a></li>
        <li><a class="nav-item current" aria-current="page" href="03-pivotroot.html"><span class="nav-num">03</span><span class="nav-label">根を挿げ替える<span class="nav-tag">Root</span></span></a></li>
        <li><a class="nav-item" href="04-cgroup.html"><span class="nav-num">04</span><span class="nav-label">資源に上限をかける<span class="nav-tag">Cgroup</span></span></a></li>
        <li><a class="nav-item" href="05-registry.html"><span class="nav-num">05</span><span class="nav-label">イメージを取ってくる<span class="nav-tag">Registry</span></span></a></li>
        <li><a class="nav-item" href="06-overlay.html"><span class="nav-num">06</span><span class="nav-label">層を重ねる<span class="nav-tag">Layer</span></span></a></li>
        <li><a class="nav-item" href="07-cli.html"><span class="nav-num">07</span><span class="nav-label">道具にする<span class="nav-tag">CLI</span></span></a></li>
        <li><a class="nav-item" href="08-network.html"><span class="nav-num">08</span><span class="nav-label">外とつながる<span class="nav-tag">Network</span></span></a></li>
        <li><a class="nav-item" href="09-beyond.html"><span class="nav-num">09</span><span class="nav-label">さらに先へ<span class="nav-tag">Beyond</span></span></a></li>
      </ul>
    </nav>

    <div class="spine-foot">
      各章は一つの層。<br>隔離する → 根を替える → 締める<br>→ 取ってくる → 重ねる、の順に積む。
    </div>
  </aside>
```

⚠ `nav-tag` は `nav-label` の**内側**（`span.nav-label > span.nav-tag`）。現在章だけ `class="nav-item current" aria-current="page"`。

### 冒頭ブロック

```html
  <main class="reading">
    <div class="progress-tag">第03章 / 全10章 · 層: ROOT · 読了目安 3〜5時間</div>
    <div class="eyebrow"><span>Root</span><span class="depth">— 世界の底を入れ替える</span></div>
    <h1>根を挿げ替える</h1>
    <p class="lede">前章で PID 名前空間を作ったのに、<code>ps</code> はまだホストのプロセスをずらりと並べます。名前空間は効いているのに、です。犯人は <code>/proc</code> でした。この章では、コンテナが立つ地面そのものを入れ替えます。</p>
```

`progress-tag` の型: `第NN章 / 全10章 · 層: LAYER · 読了目安 N〜M時間`。

### 各ボックスの実例

```html
    <div class="box goal"><div class="box-k">◈ この章のゴール</div>
      <p><code>internal/container/init.go</code> の <code>pivot_root</code> 前後を読み切り、5つの手順がなぜその順番でなければならないかを自分の言葉で説明できるようになる。仕上げに、<strong>コンテナの中で <code>ps</code> を打って、自分のプロセスだけが並ぶ</strong>ことを確認する。</p>
    </div>
```

```html
    <div class="box map"><div class="box-k">☰ この章のステップ</div>
      <ol>
        <li><a href="#step-1">まず chroot を試して、破ってみる</a></li>
        <li><a href="#step-2">伝播を切る — なぜ最初にこれが要るのか</a></li>
        <li><a href="#step-3">自分自身に bind mount する</a></li>
        <li><a href="#step-4">pivot_root と、古い根の捨て方</a></li>
        <li><a href="#step-5">/proc を貼り直す — 02章の謎が解ける</a></li>
      </ol>
    </div>
```

```html
    <h2 class="step" id="step-1"><span class="step-no">Step 1</span>まず chroot を試して、破ってみる</h2>
```

```html
    <div class="gonote"><div class="box-k">LINUX メモ · マウントの伝播（shared subtree）</div>
      <p>マウントには「伝播」という性質があります。既定では、ある名前空間で行ったマウントが別の名前空間にも伝わる（shared）ようになっていて…</p>
    </div>
```

型は `GO メモ · <イディオム名>` / `LINUX メモ · <カーネルの概念>`（中黒 `·` で区切る）。

```html
    <div class="box do"><div class="box-k">▸ 動かして確認</div>
      <p><code>/proc</code> を貼り直す前と後で、<code>ps</code> の出力を見比べてください。</p>
      <pre class="term"><code><span class="ps">/ #</span> ps
<span class="out">PID   USER     TIME  COMMAND
    1 root      0:00 /bin/sh
    5 root      0:00 ps</span></code></pre>
    </div>
```

`pre.term` の規約: `<span class="ps">$</span>`（コンテナ内なら `/ #`）がプロンプト、
出力全体を1つの `<span class="out">…</span>` で複数行まとめて包む。
⚠ **出力は実際に自分の環境で取ったものを貼る。** 想像で書かない。

```html
    <div class="box hazard"><div class="box-k">⚠ 落とし穴 · Unshareflags を忘れると、ホストのマウントが増えていく</div>
      <p><code>Unshareflags: syscall.CLONE_NEWNS</code> を指定しないと、コンテナの中で行ったマウントが<strong>ホスト側にも伝播します</strong>。しばらく気づかず、ある日 <code>mount</code> の出力が数百行になって…</p>
    </div>
```

### ASCII図

```html
    <div class="ascii">pivot_root の前後

  【前】                          【後】
  /                               /                    ← 元 rootfs
  ├ bin                           ├ bin                （イメージの中身）
  ├ home/atu/…   ← ホスト全部     ├ etc
  ├ proc         ← ホストのproc    ├ proc              ← 貼り直したもの
  └ tmp/rootfs   ← イメージ        └ .old_root         ← 元の / （この後捨てる）
      ├ bin
      └ etc</div>
```

⚠ `.ascii` は `white-space:pre` なので、**インデントを付けて書くとそのインデントも表示される**。
開始タグの直後から図を始め、閉じタグは最終行の末尾に直付けする。

### コードブロックとハイライト

```html
    <div class="file-tag">internal/container/init.go</div>
<pre><code><span class="cm">// pivotRoot は rootfs をコンテナの新しい / にする。順序を崩すと必ず失敗する。</span>
<span class="kw">func</span> pivotRoot(rootfs <span class="kw">string</span>) <span class="kw">error</span> {
	<span class="cm">// 1. マウントの伝播をホストから切る</span>
	<span class="kw">if</span> err := syscall.<span class="ty">Mount</span>(<span class="st">""</span>, <span class="st">"/"</span>, <span class="st">""</span>, syscall.<span class="ty">MS_PRIVATE</span>|syscall.<span class="ty">MS_REC</span>, <span class="st">""</span>); err != <span class="kw">nil</span> {
		<span class="kw">return</span> err
	}
	<span class="cm">// 2. 新しい root は「マウントポイント」でなければならない</span>
	<span class="kw">if</span> err := syscall.<span class="ty">Mount</span>(rootfs, rootfs, <span class="st">""</span>, syscall.<span class="ty">MS_BIND</span>|syscall.<span class="ty">MS_REC</span>, <span class="st">""</span>); err != <span class="kw">nil</span> {
		<span class="kw">return</span> err
	}
	<span class="cm">// …（3〜5 は次のステップで）</span>
}</code></pre>
```

ハイライトの規約:

| クラス | 対象 |
|---|---|
| `cm` | コメント行まるごと（`//` を含めて1つの span で包む） |
| `kw` | 予約語 **と組み込み型・組み込み関数**（`func` `type` `struct` `const` `var` `return` `if` `for` `string` `error` `byte` `int` `nil` `make` `len` `append`） |
| `ty` | ユーザー定義型・エクスポートされた識別子（`Mount` `MS_PRIVATE`）。パッケージ修飾子は span の**外** → `syscall.<span class="ty">Mount</span>` |
| `st` | 文字列リテラル **と数値リテラル両方** |
| （なし） | 通常の識別子は素のまま |

- インデントは**タブ文字**（CSS の `tab-size:4`）
- **`<pre>` タグ自体を列0に置く**
- **`<` `>` `&` は必ずエスケープ**する（`&lt;` `&amp;`）。⚠ この教材はビット OR（`|`）が頻出するが、`|` はエスケープ不要

### 章末（FAQ → 理解度チェック → 完了の合図 → まとめ → pager）

```html
    <h2>つまずき FAQ</h2>

    <details class="qa"><summary>chroot ではだめなんですか? 実際に動いてはいますよね。</summary>
      <div class="qa-body"><p>動きます。ただ、<strong>抜け出す方法が知られています</strong>。…</p></div>
    </details>

    <h2>理解度チェック</h2>
    <p>次の問いに自分の言葉で答えてから、答えを開いてください。</p>

    <details class="qa"><summary>Q1. rootfs を自分自身に bind mount するのはなぜ? これをしないと何が起きますか?</summary>
      <div class="qa-body"><p>…</p></div>
    </details>

    <details class="qa"><summary>Q2. 02章の時点で PID 名前空間は正しく作れていました。それなのに <code>ps</code> がホストのプロセスを表示したのはなぜですか?</summary>
      <div class="qa-body"><p>…</p></div>
    </details>

    <div class="box done"><div class="box-k">✓ 完了の合図</div>
      <p>コンテナの中で <code>ps</code> を打って、自分のプロセスだけが並べばこの章は完了です。</p>
      <pre class="term"><code><span class="ps">/ #</span> ps
<span class="out">PID   USER     TIME  COMMAND
    1 root      0:00 /bin/sh
    6 root      0:00 ps</span></code></pre>
    </div>

    <h2>まとめ — 次の層へ</h2>
    <p>この章で、コンテナはようやく「別の世界」になりました。…</p>
    <p>でも、この世界には<strong>底がありません</strong>。…メモリを好きなだけ確保でき、プロセスを好きなだけ増やせます。<strong>1つのコンテナがホストごと道連れにできる</strong>状態です。</p>
    <p>次の第04章では、この世界に上限を設けます。使うのは cgroup —— 名前は難しそうですが、正体はただのファイル操作です。</p>

    <div class="chap-foot">
      <a class="pager" href="02-namespace.html"><span class="dir">← 前へ</span>第02章 · 見える世界を区切る</a>
      <a class="pager next" href="04-cgroup.html"><span class="dir">次へ →</span>第04章 · 資源に上限をかける</a>
    </div>
  </main>
</div>
</body>
</html>
```

FAQ の `summary` は**読者の口調そのまま**の質問文。`?` は半角。理解度チェックは `Q1.` 始まり。
**「まとめ — 次の層へ」は必ず3段落**: ①この章で得たもの ②今のままでは何ができないか ③次章で何を建てるか。
pager のラベルは `第NN章 · タイトル`。

### 章全体の並び順

```
progress-tag → eyebrow → h1 → lede
→ box.goal → box.map
→ h2（動機づけの概念セクション。各々に .ascii を1枚）
→ table（用語表。必要なら）
→ h2.step#step-1 … #step-N
     各ステップ: 導入p → .ascii → .file-tag + pre（コード全文）
                 → 逐行解説 → .gonote → .box.hazard → .box.do(+pre.term)
→ h2 つまずき FAQ（details.qa × 4〜6）
→ h2 理解度チェック（p + details.qa × 4〜6）
→ box.done（pre.term）
→ h2 まとめ — 次の層へ（p × 3）
→ .chap-foot（pager × 2）
```

---

## 13. 最後にやること

- プロジェクト直下に `index.html` を置く（`docs/index.html` へのリダイレクトのみ）
- 最終報告では「**何が完成し、何が未完で、何が未検証か**」を正直に区別して書く。E2E の結果は**件数を実数で**出す
- **疎通実験の結果によって縮退したもの**があれば、その旨と理由を必ず書く

### 守ってほしいこと

- 文書・コミットメッセージ・コメントは**日本語**
- サブエージェントを使うときは model を指定せず親を継承させる。コスト理由の格下げをしない
- **完了報告の前に、報告する内容を実際に実行・grep して確かめる**
- ⚠ **この教材は「動くはず」が最も危険。** namespace も cgroup も overlayfs も、
  **設定が通ったこと**と**効いていること**が別物である。必ず**破って確かめる**
