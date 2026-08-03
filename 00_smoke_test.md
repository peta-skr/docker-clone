# 00 疎通実験の記録

> 実装を1行も書く前に、開発環境で**何ができて何ができないか**を実際にコマンドを打って確かめた記録。
> **出力はすべて実際に取得したものをそのまま貼っている。推測で書いた行は1つもない。**

実施日: 2026-08-03
実施環境: Claude Code のリモート実行コンテナ（Linux, root 権限あり）

---

## 実験の前に — 開発環境について

INSTRUCTIONS.md §1 は開発環境として **A案（特権 Docker コンテナ）** を既定としている。
今回の作業環境は**すでに Linux の特権コンテナの中**であり、`unshare` / `mount -t overlay` /
`mkdir /sys/fs/cgroup/...` がそのまま通る。つまり A案が求める条件は最初から満たされている。

そのため、

- **開発と実行は、この Linux 環境の中で直接行った**
- ただし**読者が同じ環境を再現できるように**、A案そのものである
  `src/devcontainer/docker-compose.yml`（`ubuntu:24.04` を `--privileged` 相当で起動）を成果物として用意した

「Docker を作るために Docker を使う」入れ子は、教材 00 章でそのまま扱う。

---

## 1. カーネルと cgroup のバージョン

```sh
uname -r
stat -fc %T /sys/fs/cgroup/
id
```

```
6.18.5
tmpfs
uid=0(root) gid=0(root) groups=0(root)
```

⚠ **`stat -fc %T /sys/fs/cgroup/` が `cgroup2fs` ではなく `tmpfs` を返した。**
INSTRUCTIONS.md の判定表では「cgroup v2 が使えない」に倒れる分岐である。
ただし**これで結論を出すのは早い**。実際にはこの環境は **cgroup v1 と v2 の混在（hybrid）構成**だった。
詳細は §3 で掘る。

---

## 2. namespace が作れるか（Must の生命線）

```sh
unshare --pid --fork --mount-proc ps aux
```

```
USER         PID %CPU %MEM    VSZ   RSS TTY      STAT START   TIME COMMAND
root           1  0.0  0.0   7900  3932 ?        R    22:14   0:00 ps aux
```

**通った。** `ps aux` が PID 1 として自分だけを表示している。
ホストで同じ `ps aux` を打つと数十行出るので、視界が区切られていることが目で分かる。

→ **namespace 隔離は Must のまま進める。** この教材が成立する。

---

## 3. cgroup のコントローラが使えるか

### 3.1 まず素直に v2 を見る（→ 失敗）

```sh
cat /sys/fs/cgroup/cgroup.controllers
cat /sys/fs/cgroup/cgroup.subtree_control
```

```
cat: /sys/fs/cgroup/cgroup.controllers: No such file or directory
cat: /sys/fs/cgroup/cgroup.subtree_control: No such file or directory
```

そもそもファイルが無い。`/sys/fs/cgroup` が cgroup2fs ではなく tmpfs だからである。

### 3.2 何がマウントされているのか調べる

```sh
ls -la /sys/fs/cgroup/
mount | grep cgroup
```

```
total 0
drwxrwxrwt 10 root root 200 Aug  2 07:26 .
drwxr-xr-x 11 root root   0 Aug  2 07:26 ..
dr-xr-xr-x  2 root root   0 Aug  2 07:26 blkio
dr-xr-xr-x  2 root root   0 Aug  2 07:26 cpu
dr-xr-xr-x  2 root root   0 Aug  2 07:26 cpuacct
dr-xr-xr-x  2 root root   0 Aug  2 07:26 devices
dr-xr-xr-x  2 root root   0 Aug  2 07:26 freezer
dr-xr-xr-x  3 root root   0 Aug  2 07:26 memory
dr-xr-xr-x  2 root root   0 Aug  2 07:26 pids
dr-xr-xr-x  2 root root   0 Aug  2 07:26 unified
```

```
tmpfs on /sys/fs/cgroup type tmpfs (rw,relatime)
cgroup on /sys/fs/cgroup/cpu type cgroup (rw,relatime,cpu)
cgroup on /sys/fs/cgroup/cpuacct type cgroup (rw,relatime,cpuacct)
cgroup on /sys/fs/cgroup/memory type cgroup (rw,relatime,memory)
cgroup on /sys/fs/cgroup/devices type cgroup (rw,relatime,devices)
cgroup on /sys/fs/cgroup/freezer type cgroup (rw,relatime,freezer)
cgroup on /sys/fs/cgroup/blkio type cgroup (rw,relatime,blkio)
cgroup on /sys/fs/cgroup/pids type cgroup (rw,relatime,pids)
cgroup2 on /sys/fs/cgroup/unified type cgroup2 (rw,relatime)
```

**判明したこと**: `/sys/fs/cgroup` は tmpfs の「置き場」で、その下に
**v1 のコントローラが1つずつ別マウント**され、さらに `unified/` に **cgroup2 も同時にマウント**されている。
これが典型的な **hybrid 構成**である。

`/proc/self/cgroup` も v1 と v2 の両方の行を持っている:

```
7:pids:/
6:blkio:/
5:freezer:/
4:devices:/
3:memory:/process_api/019fc9b0-aeea-7122-9c35-01e1302d5859
2:cpuacct:/
1:cpu:/
0::/
```

（`0::/` の行が cgroup v2 のもの。数字付きの行が v1。）

### 3.3 v2 側で memory / cpu / pids が使えるか（→ 使えない）

```sh
cat /sys/fs/cgroup/unified/cgroup.controllers
cat /sys/fs/cgroup/unified/cgroup.subtree_control
```

```
cpuset hugetlb

```

（`subtree_control` は空行）

念のため、自前で新しい cgroup2 をマウントしても同じか確かめた:

```sh
mkdir -p /tmp/cg2 && mount -t cgroup2 none /tmp/cg2
cat /tmp/cg2/cgroup.controllers
```

```
cpuset hugetlb
```

⚠ **これが今回いちばん重要な発見。**
cgroup2 自体はマウントできる。しかし **`memory` / `cpu` / `pids` は v2 側に見えない**。
hybrid 構成では、**あるコントローラは v1 か v2 のどちらか一方にしか結び付けられない**。
この環境では `memory` / `cpu` / `pids` は **v1 に取られている**ので、v2 からは使えない。

→ **v2 だけで書いた実装は、この環境では「`memory.max` が作られない」という形で静かに失敗する。**

### 3.4 v1 側なら書けるか（→ 書ける）

```sh
mkdir -p /sys/fs/cgroup/memory/smoketest
echo 10485760 > /sys/fs/cgroup/memory/smoketest/memory.limit_in_bytes
cat /sys/fs/cgroup/memory/smoketest/memory.limit_in_bytes

mkdir -p /sys/fs/cgroup/pids/smoketest
echo 64 > /sys/fs/cgroup/pids/smoketest/pids.max
cat /sys/fs/cgroup/pids/smoketest/pids.max
```

```
10485760
64
```

`cpu` も `cpu.cfs_quota_us` / `cpu.cfs_period_us` が揃っている:

```
cgroup.clone_children
cgroup.procs
cgroup.sane_behavior
cpu.cfs_burst_us
cpu.cfs_period_us
cpu.cfs_quota_us
cpu.idle
cpu.rt_period_us
cpu.rt_runtime_us
cpu.shares
```

---

## 4. 上限は「書けた」だけでなく「効く」か（★ここを確かめないと意味がない）

**`cat memory.limit_in_bytes` が 10485760 を返すことは、上限が効いている証拠にはならない。**
実際に超過させて殺されるかを見る。10MB 制限の cgroup に自分を入れ、200MB 確保しにいく Python を書いた:

```python
import os
open("/sys/fs/cgroup/memory/smoketest/cgroup.procs","w").write(str(os.getpid()))
b = bytearray()
for i in range(200):
    b += bytearray(1024*1024)
print("allocated", len(b))
```

```sh
python3 /tmp/hog.py; echo "exit=$?"
cat /sys/fs/cgroup/memory/smoketest/memory.failcnt
```

```
/bin/bash: line 32:  5187 Killed                  python3 /tmp/hog.py
exit=137
37
```

**効いた。** `allocated ...` は**印字されずに** `Killed`（`exit=137` = 128+SIGKILL(9)）で終わった。
`memory.failcnt` が 37 に増えており、上限に当たった回数がカーネル側にも記録されている。

→ **cgroup による資源制限は Must のまま進められる。ただし v1 で。**

---

## 5. overlayfs がマウントできるか

```sh
mkdir -p /tmp/ovl/{lower,upper,work,merged}
mount -t overlay overlay -o lowerdir=/tmp/ovl/lower,upperdir=/tmp/ovl/upper,workdir=/tmp/ovl/work /tmp/ovl/merged
mount | grep overlay
```

```
overlay on /tmp/ovl/merged type overlay (rw,relatime,lowerdir=/tmp/ovl/lower,upperdir=/tmp/ovl/upper,workdir=/tmp/ovl/work,uuid=on)
```

**通った。** → 層の重ね合わせは **Should** のまま進める。

---

## 6. Docker Hub からイメージが取れるか

### 6.1 トークン

```sh
curl -s "https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/alpine:pull" | head -c 200
```

```
{"token":"eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCIsIng1YyI6WyJNSUlFRmpDQ0F2NmdBd0lCQWdJVU9yTFd5UVpxMmFuZXd6WnhYN1JLbHQ3bDVUTXdEUVlKS29aSWh2Y05BUUVMQlFBd2dZWXhDekFKQmdOVkJBWVRBbFZUTVJNd0VRWURWUVFJRXdwRFlXeH
```

取れた（トークン全体は 2658 文字）。

### 6.2 マニフェスト — ⚠ いきなり落とし穴を踏んだ

```sh
curl -s -H "Authorization: Bearer $TOKEN" \
  -H "Accept: application/vnd.docker.distribution.manifest.v2+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.oci.image.index.v1+json" \
  "https://registry-1.docker.io/v2/library/alpine/manifests/latest"
```

```json
{"manifests":[{"annotations":{"com.docker.official-images.bashbrew.arch":"amd64","org.opencontainers.image.base.name":"scratch","org.opencontainers.image.created":"2026-06-16T00:01:04Z","org.opencontainers.image.revision":"398ff0c866d27e9f46f53e48184fe36c674b8897","org.opencontainers.image.source":"https:\/\/github.com\/alpinelinux\/docker-alpine.git#398ff0c866d27e9f46f53e48184fe36c674b8897:x86_64","org.opencontainers.image.url":"https:\/\/hub.docker.com\/_\/alpine","org.opencontainers.image.version":"3.24.1"},"digest":"sha256:79ff19e9084a00eece421b2523fb93e22d730e2c0e525905de047e848e56d95f","mediaType":"application\/vnd.oci.image.manifest.v1+json","platform":{"architecture":"amd64","os":"linux"},"size":1022},{"annotations":{...,"vnd.docker.reference.type":"attestation-manifest"},"digest":"sha256:f21951a6120df0f5f9329311202f7869e7b70120f4748632d58753cff662b126","mediaType":"application\/vnd.oci.image.manifest.v1+json","platform":{"architecture":"unknown","os":"unknown"},"size":838},...
```

⚠ **`layers` が無い。** 返ってきたのは **OCI イメージインデックス（マニフェストリスト）** だった。
`platform.architecture == "amd64"` かつ `platform.os == "linux"` の項目を選び、
その `digest` でもう一度取り直す必要がある。

さらに、リストの中には `platform.architecture == "unknown"` で
`vnd.docker.reference.type: attestation-manifest`（署名の付随物）が混ざっている。
**「最初の項目を取る」実装は、この attestation を掴んで確実に壊れる。**
アーキテクチャと OS で明示的に絞る必要がある。

→ イメージ取得は **Should** のまま進める。

---

## 7. 後片付け（★これをやらないと筋が通らない）

この教材は「後片付けができているか」を検査する教材である。その入口が汚したままでは話にならない。

```sh
# 5 の後片付け。★アンマウントが成功したときだけ削除する
umount /tmp/ovl/merged
if mountpoint -q /tmp/ovl/merged; then
	echo "!! まだマウントされている。削除しない。"
else
	rm -rf /tmp/ovl
fi

umount /tmp/cg2 && rmdir /tmp/cg2

# 4 / 3 の後片付け（cgroupfs は rm -rf では消えない。rmdir を使う）
rmdir /sys/fs/cgroup/unified/smoketest
rmdir /sys/fs/cgroup/memory/smoketest
rmdir /sys/fs/cgroup/pids/smoketest
```

片付いたことの確認:

```sh
mount | grep -c /tmp/ovl
ls -d /sys/fs/cgroup/memory/smoketest
ls -d /sys/fs/cgroup/unified/smoketest
```

```
unmounted and removed
=== 確認 ===
mount|grep -c /tmp/ovl : 0
ls: cannot access '/sys/fs/cgroup/memory/smoketest': No such file or directory
ls: cannot access '/sys/fs/cgroup/unified/smoketest': No such file or directory
```

**残っていない。** overlay のマウントは 0 件、cgroup ディレクトリは両方とも消えている。

なお `memory.failcnt` が 37 まで上がっていた `smoketest` は、
**中のプロセスが既に OOM で死んでいた**ので `rmdir` が素直に成功した。
プロセスが生きていれば `rmdir` は `EBUSY` で失敗する——これは**正しい挙動**であり、
07 章の後片付け実装で正面から扱う。

---

## 8. 判定 — スコープの確定

| 確認項目 | 結果 | 判定 |
|---|---|---|
| namespace（§2） | ✅ 通った | **Must のまま** |
| cgroup（§3, §4） | ⚠ **v2 では不可 / v1 なら効く** | **Must のまま。ただし実装を v2/v1 両対応にする**（下記） |
| overlayfs（§5） | ✅ 通った | **Should のまま** |
| Registry（§6） | ✅ 通った（マニフェストリスト対応が必須） | **Should のまま** |

### ⚠ 縮退ではなく「拡張」した点 — cgroup を v1/v2 両対応にする

INSTRUCTIONS.md §1 の判定表は「cgroup v2 が落ちたら **Should に降格**し、
04 章は『なぜ効かない環境があるか』を書く章にする」としている。

しかし §4 で確かめたとおり、**この環境でも v1 経由なら上限は実際に効く**。
ここで降格すると、**動かせるものを動かさずに終わる**ことになる。そこで次の判断をした:

- **cgroup は Must のまま維持する**
- `internal/cgroup` を **v2 優先・v1 フォールバック**の2バックエンド構成にする
  - 起動時に「v2 の統一階層に `memory` コントローラが見えるか」を判定する
  - 見えれば v2（`memory.max` / `cpu.max` / `pids.max`）
  - 見えなければ v1（`memory.limit_in_bytes` / `cpu.cfs_quota_us` / `pids.max`）
- **04 章は「なぜ効かない環境があるか」も併せて書く**。
  実際に踏んだ hybrid 構成の話は、机上の一般論よりずっと良い教材になる

**この判断により落としたものは無い。** 実装コストは 1 ファイル分増えたが、
「設定できた」と「効いた」の差を読者に見せるという教材の核が守られる。

### 縮退したもの

| 項目 | 判断 | 理由 |
|---|---|---|
| **ネットワーク（veth / bridge / NAT）** | **未実装（Could のまま落とす）** | INSTRUCTIONS.md §2 が Could に置き「Must と Should が終わるまで手を出さない」と明記している。Must と Should を検証込みで仕上げることを優先した。08 章は「なぜ難しいか・何が必要か」の地図として書く |

---

## 9. この実験で先に分かった「落とし穴」

実装を書く前の 60 分で、**教材の落とし穴ボックスに入れるネタが4つ**手に入った。

1. **`stat -fc %T /sys/fs/cgroup/` が `tmpfs` でも cgroup v2 が無いとは限らない** — hybrid 構成では
   tmpfs の下に v1 と v2 が同居する。`mount | grep cgroup` まで見ないと判断を誤る
2. **cgroup2 がマウントできても `memory` が使えるとは限らない** — コントローラは v1/v2 のどちらか
   一方にしか結び付かない。`cgroup.controllers` を読んで確かめること
3. **`cat memory.limit_in_bytes` が期待値を返しても、効いている証明にはならない** — 超過させて
   `Killed`（exit 137）を見るまでは信じない
4. **マニフェストの取得は1回では終わらない** — 返ってくるのはインデックスで、`layers` は無い。
   しかも中に `architecture: unknown` の attestation が混ざっている。
   「最初の項目を取る」実装は確実に壊れる
