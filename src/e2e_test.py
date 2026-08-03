# -*- coding: utf-8 -*-
"""自作コンテナランタイム mydocker E2E テスト（要件の受け入れ基準に沿う）

実行方法（★特権 Linux 環境の中で実行すること）:

    docker compose -f devcontainer/docker-compose.yml run --rm dev \
        sh -c 'go build -o mydocker ./cmd/mydocker && python3 e2e_test.py'

使うイメージは環境変数 MYDOCKER_TEST_IMAGE で差し替えられる。
（Docker Hub のレイヤ配信ドメインに出られない環境があるため）
"""
import os
import subprocess
import sys
import tarfile
import time

BIN = "./mydocker"
IMAGE = os.environ.get("MYDOCKER_TEST_IMAGE", "alpine")
WORK = "/tmp/mydocker-e2e"

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


def run(args, timeout=120):
    """mydocker を起動し (returncode, stdout, stderr) を返す。例外にしない。"""
    try:
        p = subprocess.run([BIN] + args, capture_output=True, text=True, timeout=timeout)
        return p.returncode, p.stdout, p.stderr
    except subprocess.TimeoutExpired:
        return -1, "", "TIMEOUT"


def sh(cmd, timeout=60):
    """ホスト側のコマンド（検査用）。"""
    p = subprocess.run(cmd, shell=True, capture_output=True, text=True, timeout=timeout)
    return p.returncode, p.stdout, p.stderr


# ---------------------------------------------------------------------------
# 検査用の小道具
# ---------------------------------------------------------------------------

def count_overlay_mounts():
    """ホストに存在する overlay マウントの数を数える。リーク検出の物差し。"""
    _, out, _ = sh("grep -c ' overlay ' /proc/mounts || true")
    try:
        return int(out.strip())
    except ValueError:
        return 0


def count_host_mounts():
    _, out, _ = sh("wc -l < /proc/mounts")
    return int(out.strip())


def cgroup_leftovers():
    """/sys/fs/cgroup 配下に残った mydocker のディレクトリを列挙する。"""
    _, out, _ = sh("find /sys/fs/cgroup -maxdepth 4 -type d -name mydocker 2>/dev/null "
                   "-exec find {} -mindepth 1 -maxdepth 1 -type d \\; || true")
    return [l for l in out.strip().split("\n") if l.strip()]


def container_dirs():
    _, out, _ = sh("ls -1 /var/lib/mydocker/containers 2>/dev/null || true")
    return [l for l in out.strip().split("\n") if l.strip()]


def make_layer(path, files, whiteouts=(), opaques=()):
    """tar.gz のレイヤを1枚こしらえる。files は {パス: 中身}。"""
    import io
    with tarfile.open(path, "w:gz") as tf:
        for name, content in files.items():
            data = content.encode()
            info = tarfile.TarInfo(name)
            info.size = len(data)
            info.mode = 0o644
            tf.addfile(info, io.BytesIO(data))
        for name in whiteouts:
            info = tarfile.TarInfo(name)
            info.size = 0
            tf.addfile(info, io.BytesIO(b""))
        for name in opaques:
            info = tarfile.TarInfo(name)
            info.size = 0
            tf.addfile(info, io.BytesIO(b""))


def pull_layer_dirs(image):
    """mydocker pull の出力から、展開済みレイヤのディレクトリを取り出す。"""
    code, out, err = run(["pull", image], timeout=300)
    if code != 0:
        return None, (out + err)
    dirs = []
    for line in out.split("\n"):
        line = line.strip()
        if line and line[0].isdigit() and ": /" in line:
            dirs.append(line.split(": ", 1)[1])
    return dirs, out


# ===========================================================================

print("mydocker E2E テスト")
print(f"  バイナリ : {BIN}")
print(f"  イメージ : {IMAGE}")
print()

sh(f"rm -rf {WORK} && mkdir -p {WORK}")

host_mounts_at_start = count_host_mounts()
overlay_at_start = count_overlay_mounts()
_, host_hostname, _ = sh("hostname")
host_hostname = host_hostname.strip()

print("== 0. 準備 — イメージの取得 ==")
layer_dirs, pull_out = pull_layer_dirs(IMAGE)
check("イメージを取得できる (FR-5)", layer_dirs is not None and len(layer_dirs) > 0,
      f"出力: {pull_out[:300]}")
if not layer_dirs:
    print("\nイメージを取得できないため、以降のテストを実行できません。")
    print(f"RESULT: {ok_count} passed, {fail_count + 1} failed")
    sys.exit(1)
print(f"       （レイヤ {len(layer_dirs)} 枚）")
print()

# ---------------------------------------------------------------------------
print("== 1. コンテナの起動とコマンドの実行 (FR-1) ==")

code, out, err = run(["run", "--quiet", IMAGE, "/bin/echo", "hello-from-container"])
check("指定したコマンドが実行され、その出力が手元に届く",
      code == 0 and "hello-from-container" in out, f"code={code} out={out!r} err={err!r}")

code, out, err = run(["run", "--quiet", IMAGE, "/bin/sh", "-c", "exit 42"])
check("コンテナ内のコマンドの終了コードがそのまま返る", code == 42, f"code={code}")

code, out, err = run(["run", "--quiet", IMAGE, "/no/such/command"])
check("存在しないコマンドを指定するとエラーになる (異常系)",
      code != 0 and "見つかりません" in err, f"code={code} err={err!r}")

# コマンドを省略したら、イメージが持つ既定のコマンドで起動する
p = subprocess.run([BIN, "run", "--quiet", IMAGE], input="echo default-cmd-ok\nexit\n",
                   capture_output=True, text=True, timeout=60)
check("コマンドを省略するとイメージの既定コマンドで起動する",
      "default-cmd-ok" in p.stdout, f"out={p.stdout!r} err={p.stderr!r}")

# ローカルのディレクトリを根として使う（レジストリを使わない経路）
code, out, err = run(["run", "--quiet", "--rootfs", layer_dirs[-1], "/bin/echo", "rootfs-ok"])
check("ローカルの rootfs を指定して起動できる (--rootfs)",
      code == 0 and "rootfs-ok" in out, f"code={code} out={out!r} err={err!r}")

code, out, err = run(["run", "--quiet", "--rootfs", layer_dirs[-1], "/bin/sh", "-c",
                      "echo dirty > /rootfs-scribble.txt"])
check("--rootfs で起動しても、指定した元のディレクトリが汚れない",
      code == 0 and not os.path.exists(os.path.join(layer_dirs[-1], "rootfs-scribble.txt")),
      f"code={code} err={err!r}")
print()

# ---------------------------------------------------------------------------
print("== 2. ホストから隔離された視界 (FR-2, FR-3) ==")

code, out, err = run(["run", "--quiet", IMAGE, "/bin/ps"])
ps_lines = [l for l in out.strip().split("\n") if l.strip()]
# 期待: ヘッダ + PID 1(ps 自身) の 2 行ちょうど。
# ★「0 行でない」で済ませない。ホストのプロセスが混ざれば行数は必ず増える。
check("コンテナ内の ps にホストのプロセスが1つも出ない（行数ちょうど）",
      len(ps_lines) == 2, f"{len(ps_lines)} 行: {ps_lines!r}")

_, host_ps, _ = sh("ps -e --no-headers | wc -l")
print(f"       （ホスト側の実行中プロセスは {host_ps.strip()} 個。コンテナ内は 1 個）")

check("コンテナ内で最初に起動したプロセスが PID 1 である",
      len(ps_lines) >= 2 and ps_lines[1].strip().startswith("1 "),
      f"2行目: {ps_lines[1] if len(ps_lines) > 1 else '(なし)'!r}")

code, out, err = run(["run", "--quiet", "--hostname", "isolated-box", IMAGE, "/bin/hostname"])
check("コンテナ内の hostname がホストと違う",
      code == 0 and out.strip() == "isolated-box" and out.strip() != host_hostname,
      f"コンテナ={out.strip()!r} ホスト={host_hostname!r}")

code, out, err = run(["run", "--quiet", IMAGE, "/bin/ls", "/"])
entries = set(out.split())
check("コンテナ内の / がイメージの中身である",
      "etc" in entries and "bin" in entries, f"ls / = {sorted(entries)!r}")

# ホスト側にしか無いはずのディレクトリが見えていないことを、実際のホストの / と突き合わせる
_, host_root, _ = sh("ls -1 /")
host_only = set(host_root.split()) - entries - {"proc", "sys", "dev"}
check("ホストにしか無いディレクトリがコンテナから見えない (FR-3)",
      len(host_only) > 0 and not (host_only & entries),
      f"ホスト固有={sorted(host_only)!r}")

code, out, err = run(["run", "--quiet", IMAGE, "/bin/sh", "-c", "ls /.old_root 2>&1; echo rc=$?"])
check("pivot_root で押し出した古い root が残っていない",
      "rc=0" not in out, f"out={out!r}")

code, out, err = run(["run", "--quiet", IMAGE, "/bin/sh", "-c",
                      "grep -c ' / ' /proc/mounts"])
check("コンテナ内の /proc がコンテナ自身のものを指している",
      code == 0 and out.strip().isdigit(), f"out={out!r}")
print()

# ---------------------------------------------------------------------------
print("== 3. 資源の上限 (FR-4) ==")

# ★「memory.max に書けた」で通してはいけない。実際に超過させて殺されることを見る。
code_free, out_free, err_free = run(
    ["run", "--quiet", IMAGE, "/bin/sh", "-c", "dd if=/dev/zero of=/dev/null bs=100M count=1"])
check("上限なしなら 100MB の確保に成功する（対照）",
      code_free == 0, f"code={code_free} err={err_free!r}")

code_lim, out_lim, err_lim = run(
    ["run", "--quiet", "--memory", "10m", IMAGE, "/bin/sh", "-c",
     "dd if=/dev/zero of=/dev/null bs=100M count=1"])
# 128+SIGKILL(9) = 137。強制終了されたか、確保に失敗して非ゼロで終わればよい。
check("メモリ上限 10m を超える確保が実際に失敗する（設定値の確認では済ませない）",
      code_lim != 0, f"code={code_lim} out={out_lim!r} err={err_lim!r}")
check("上限超過が OOM による強制終了として報告される (exit 137)",
      code_lim == 137, f"code={code_lim}")

code, out, err = run(["run", "--quiet", "--pids", "8", IMAGE, "/bin/sh", "-c", "echo pids-ok"])
check("プロセス数の上限を指定して起動できる", code == 0 and "pids-ok" in out, f"code={code} err={err!r}")

code, out, err = run(["run", "--quiet", "--cpus", "0.5", IMAGE, "/bin/sh", "-c", "echo cpu-ok"])
check("CPU の上限を指定して起動できる", code == 0 and "cpu-ok" in out, f"code={code} err={err!r}")

left = cgroup_leftovers()
check("コンテナ終了後、cgroup のディレクトリが1つも残らない (FR-10)",
      len(left) == 0, f"残存: {left!r}")
print()

# ---------------------------------------------------------------------------
print("== 4. イメージの取得とキャッシュ (FR-5) ==")

code, out, err = run(["pull", IMAGE], timeout=300)
check("2回目の取得はキャッシュから行われ、ネットワークにアクセスしない",
      code == 0 and "キャッシュ" in out, f"out={out[:300]!r}")

code, out, err = run(["run", "--quiet", "no-such-image-xyzzy-12345", "/bin/sh"], timeout=120)
check("存在しないイメージを指定するとエラーになる (異常系)",
      code != 0 and len(err.strip()) > 0, f"code={code} err={err[:200]!r}")
print()

# ---------------------------------------------------------------------------
print("== 5. 取得したイメージの安全な展開 (FR-6) ==")

# ★「弾いているはず」で済ませない。細工した tar を自分で作って投入する。
evil_rel = f"{WORK}/evil-relative.tar.gz"
evil_abs = f"{WORK}/evil-absolute.tar.gz"
make_layer(evil_rel, {"../../../../tmp/mydocker-escaped-relative.txt": "escaped!"})
make_layer(evil_abs, {"/tmp/mydocker-escaped-absolute.txt": "escaped!"})

sh("rm -f /tmp/mydocker-escaped-relative.txt /tmp/mydocker-escaped-absolute.txt")

code, out, err = run(["unpack", evil_rel, f"{WORK}/unpack-rel"])
check("'../' を含む tar は展開が拒否される",
      code != 0 and "外を指しています" in err, f"code={code} err={err!r}")
check("'../' を含む tar が展開先の外にファイルを作れない",
      not os.path.exists("/tmp/mydocker-escaped-relative.txt"))

code, out, err = run(["unpack", evil_abs, f"{WORK}/unpack-abs"])
check("絶対パスを含む tar は展開が拒否される",
      code != 0 and "外を指しています" in err, f"code={code} err={err!r}")
check("絶対パスを含む tar が展開先の外にファイルを作れない",
      not os.path.exists("/tmp/mydocker-escaped-absolute.txt"))

good = f"{WORK}/good.tar.gz"
make_layer(good, {"hello.txt": "hi"})
code, out, err = run(["unpack", good, f"{WORK}/unpack-good"])
check("正常な tar は展開できる（弾きすぎていないことの確認）",
      code == 0 and os.path.exists(f"{WORK}/unpack-good/hello.txt"), f"code={code} err={err!r}")
print()

# ---------------------------------------------------------------------------
print("== 6. レイヤの重ね合わせ (FR-7) ==")

l1_tar, l2_tar = f"{WORK}/l1.tar.gz", f"{WORK}/l2.tar.gz"
l1_dir, l2_dir = f"{WORK}/layer1", f"{WORK}/layer2"
make_layer(l1_tar, {
    "probe/keep.txt": "keep",
    "probe/gone.txt": "この行は上の層で消される",
    "probe/version.txt": "v1",
})
make_layer(l2_tar, {"probe/version.txt": "v2"}, whiteouts=["probe/.wh.gone.txt"])

sh(f"rm -rf {l1_dir} {l2_dir}")
c1, _, e1 = run(["unpack", l1_tar, l1_dir])
c2, _, e2 = run(["unpack", l2_tar, l2_dir])
check("2枚のレイヤを展開できる", c1 == 0 and c2 == 0, f"{e1!r} {e2!r}")

base = layer_dirs[-1]
overlay_args = ["run", "--quiet"]
for d in layer_dirs:
    overlay_args += ["--layer", d]
overlay_args += ["--layer", l1_dir, "--layer", l2_dir]

code, out, err = run(overlay_args + ["/bin/cat", "/probe/version.txt"])
check("後のレイヤの内容が先のレイヤより優先して見える（lowerdir の順序）",
      code == 0 and out.strip() == "v2", f"code={code} out={out!r} err={err!r}")

code, out, err = run(overlay_args + ["/bin/cat", "/probe/keep.txt"])
check("上の層で触っていないファイルは下の層のまま見える",
      code == 0 and out.strip() == "keep", f"code={code} out={out!r}")

code, out, err = run(overlay_args + ["/bin/sh", "-c", "ls /probe/gone.txt 2>&1; echo rc=$?"])
check("上の層で削除されたファイルが見えない（whiteout の検証）",
      "rc=0" not in out, f"out={out!r}")

code, out, err = run(overlay_args + ["/bin/sh", "-c", "echo written > /probe/scribble.txt && cat /probe/scribble.txt"])
check("コンテナ内でファイルを書き込める（upperdir が効いている）",
      code == 0 and "written" in out, f"code={code} out={out!r} err={err!r}")

check("書き込みが元のレイヤを汚していない（lowerdir は読み取り専用）",
      not os.path.exists(f"{l2_dir}/probe/scribble.txt"))

code, out, err = run(overlay_args + ["/bin/sh", "-c", "ls /probe/scribble.txt 2>&1; echo rc=$?"])
check("別のコンテナから起動すると前のコンテナの書き込みが見えない（使い捨て）",
      "rc=0" not in out, f"out={out!r}")
print()

# ---------------------------------------------------------------------------
print("== 7. 実行中コンテナの一覧 (FR-8) と追加コマンド実行 (FR-9) ==")

code, out, err = run(["ps"])
check("コンテナが動いていないとき ps はちょうど 0 件を表示する",
      "合計 0 件" in out, f"out={out!r}")

bg1 = subprocess.Popen([BIN, "run", "--quiet", "--hostname", "box-a", IMAGE, "sleep", "25"],
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
bg2 = subprocess.Popen([BIN, "run", "--quiet", "--hostname", "box-b", IMAGE, "sleep", "25"],
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

deadline = time.time() + 20
listed = ""
while time.time() < deadline:
    _, listed, _ = run(["ps"])
    if "合計 2 件" in listed:
        break
    time.sleep(0.3)

# ★「0 件でない」ではなく「ちょうど 2 件」を見る
check("実行中のコンテナがちょうど期待件数（2件）表示される",
      "合計 2 件" in listed, f"ps 出力:\n{listed}")

rows = [l for l in listed.strip().split("\n")[1:-1] if l.strip()]
check("一覧に識別子・イメージ名・コマンドが並ぶ",
      len(rows) == 2 and all(IMAGE.split("/")[-1] in r and "sleep 25" in r for r in rows),
      f"rows={rows!r}")

cid = rows[0].split()[0] if rows else ""

code, out, err = run(["exec", cid, "/bin/hostname"])
check("実行中のコンテナの中でコマンドを実行できる",
      code == 0 and out.strip() in ("box-a", "box-b"), f"code={code} out={out!r} err={err!r}")

code, out, err = run(["exec", cid, "/bin/ps"])
exec_ps = [l for l in out.strip().split("\n") if l.strip()]
check("exec したコマンドからコンテナと同じプロセス一覧が見える（PID 1 は sleep）",
      code == 0 and len(exec_ps) >= 2 and "sleep" in exec_ps[1], f"out={out!r}")
check("exec の ps にもホストのプロセスが出ない",
      len(exec_ps) <= 4, f"{len(exec_ps)} 行: {exec_ps!r}")

code, out, err = run(["exec", "deadbeef9999", "/bin/echo", "x"])
check("実行中でない識別子を指定するとエラーになる (異常系)",
      code != 0 and "見つかりません" in err, f"code={code} err={err!r}")

# ★使用中のコンテナを削除しようとしても、削除に進まないこと。
#   ここでイメージの実体（lowerdir）が消えると、この教材で最も痛い事故になる。
probe_file = os.path.join(base, "etc")
had_image = os.path.exists(probe_file)
code, out, err = run(["rm", cid])
check("実行中のコンテナは rm できない（アンマウント失敗時に削除へ進まない）",
      code != 0 and "実行中" in err, f"code={code} err={err!r}")
check("rm を拒否した後もイメージの実体が無事である",
      had_image and os.path.exists(probe_file), f"{probe_file}")

bg1.wait(timeout=60)
bg2.wait(timeout=60)

code, out, err = run(["ps"])
check("終了したコンテナは一覧に表示されない（ちょうど 0 件）",
      "合計 0 件" in out, f"out={out!r}")
print()

# ---------------------------------------------------------------------------
print("== 8. 後片付けとリーク (FR-10) ==")
# ★このセクションは他の項目の状態を壊すので最後に置く。

before = count_overlay_mounts()
N = 10
codes = []
for i in range(N):
    c, _, _ = run(["run", "--quiet", IMAGE, "/bin/true"])
    codes.append(c)
after = count_overlay_mounts()
check(f"{N} 回連続で起動・終了してもすべて成功する（実数で確認: {codes.count(0)}/{N} 件成功）",
      codes.count(0) == N, f"codes={codes!r}")
check(f"{N} 回起動しても overlay のマウント数が増えない（前 {before} 件 → 後 {after} 件）",
      after == before, f"before={before} after={after}")

# 起動の途中で失敗させる（存在しないコマンド）
fail_before = count_overlay_mounts()
for _ in range(3):
    run(["run", "--quiet", "--memory", "16m", IMAGE, "/definitely/not/here"])
fail_after = count_overlay_mounts()
check(f"起動に失敗したときも overlay マウントが残らない（前 {fail_before} 件 → 後 {fail_after} 件）",
      fail_after == fail_before, f"before={fail_before} after={fail_after}")
check("起動に失敗したときも cgroup が残らない",
      len(cgroup_leftovers()) == 0, f"残存: {cgroup_leftovers()!r}")
check("起動に失敗したときもコンテナのディレクトリが残らない",
      len(container_dirs()) == 0, f"残存: {container_dirs()!r}")

# ★mydocker 自身が強制終了された場合の残骸を、rm が安全に回収できること。
#   このとき overlay はマウントされたまま残っている。順序を誤ると
#   マウント越しにイメージの実体まで消えるので、ここが最も危ない経路である。
bg = subprocess.Popen([BIN, "run", "--quiet", IMAGE, "sleep", "60"],
                      stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
deadline = time.time() + 20
while time.time() < deadline and not container_dirs():
    time.sleep(0.2)
leftover = container_dirs()
child_pid = 0
if leftover:
    import json
    with open(f"/var/lib/mydocker/containers/{leftover[0]}/state.json") as f:
        child_pid = json.load(f)["pid"]
bg.kill()          # 親（mydocker）を強制終了 → 後片付けが走らない
bg.wait(timeout=20)
sh(f"kill -9 {child_pid} 2>/dev/null || true")
deadline = time.time() + 20
while time.time() < deadline and sh(f"kill -0 {child_pid} 2>/dev/null")[0] == 0:
    time.sleep(0.2)

check("mydocker を強制終了すると、マウントと記録が残骸として残る（前提の確認）",
      len(leftover) == 1 and count_overlay_mounts() > overlay_at_start,
      f"残骸={leftover!r} overlay={count_overlay_mounts()}")

code, out, err = run(["ps"])
check("死んだコンテナは ps に出ない（記録が残っていても）",
      "合計 0 件" in out, f"out={out!r}")

image_probe = os.path.join(layer_dirs[-1], "etc")
code, out, err = run(["rm", leftover[0]] if leftover else ["rm", "none"])
check("残骸を rm で回収できる（アンマウントしてから削除する）",
      code == 0, f"code={code} err={err!r}")
check("回収後、overlay マウントも記録も残らない",
      count_overlay_mounts() == overlay_at_start and len(container_dirs()) == 0,
      f"overlay={count_overlay_mounts()} dirs={container_dirs()!r}")
check("回収の過程でイメージの実体が消えていない",
      os.path.isdir(image_probe), f"{image_probe}")

host_mounts_at_end = count_host_mounts()
check(f"テスト全体を通してホストのマウント数が元に戻る（開始 {host_mounts_at_start} → 終了 {host_mounts_at_end}）",
      host_mounts_at_end == host_mounts_at_start,
      f"start={host_mounts_at_start} end={host_mounts_at_end}")
check(f"テスト全体を通して overlay マウントが元に戻る（開始 {overlay_at_start} → 終了 {count_overlay_mounts()}）",
      count_overlay_mounts() == overlay_at_start)

sh(f"rm -rf {WORK}")
print()

print(f"RESULT: {ok_count} passed, {fail_count} failed")
sys.exit(1 if fail_count else 0)
