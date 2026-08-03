# -*- coding: utf-8 -*-
"""教材サイトの機械的な品質チェック（docs/_引き継ぎ指示.md の「完成後の品質チェック」を実行する）

    python3 docs/_check.py

すべて合格すれば終了コード 0、1つでも落ちれば 1 を返す。
"""
import os
import re
import subprocess
import sys

DOCS = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(DOCS)
SRC = os.path.join(ROOT, "src")

PAGES = [
    "index.html", "00-env.html", "01-reexec.html", "02-namespace.html",
    "03-pivotroot.html", "04-cgroup.html", "05-registry.html", "06-overlay.html",
    "07-cli.html", "08-network.html", "09-beyond.html",
]

# pager の期待する連結（前へ, 次へ）。None は「置かない」
PAGER = {
    "index.html": (None, "00-env.html"),
    "00-env.html": ("index.html", "01-reexec.html"),
    "01-reexec.html": ("00-env.html", "02-namespace.html"),
    "02-namespace.html": ("01-reexec.html", "03-pivotroot.html"),
    "03-pivotroot.html": ("02-namespace.html", "04-cgroup.html"),
    "04-cgroup.html": ("03-pivotroot.html", "05-registry.html"),
    "05-registry.html": ("04-cgroup.html", "06-overlay.html"),
    "06-overlay.html": ("05-registry.html", "07-cli.html"),
    "07-cli.html": ("06-overlay.html", "08-network.html"),
    "08-network.html": ("07-cli.html", "09-beyond.html"),
    "09-beyond.html": ("08-network.html", None),
}

MIN_SIZE = 57 * 1024

ok = 0
fail = 0


def check(name, cond, detail=""):
    global ok, fail
    if cond:
        ok += 1
        print(f"  OK   {name}")
    else:
        fail += 1
        print(f"  FAIL {name} {detail}")


def read(page):
    with open(os.path.join(DOCS, page), encoding="utf-8") as f:
        return f.read()


print("教材サイトの品質チェック")
print()

# ---------------------------------------------------------------------------
print("== 1. ページが揃っているか ==")
missing = [p for p in PAGES if not os.path.exists(os.path.join(DOCS, p))]
check(f"全11ページが存在する（{len(PAGES) - len(missing)}/{len(PAGES)}）", not missing, f"欠け: {missing}")
check("style.css が存在する", os.path.exists(os.path.join(DOCS, "style.css")))
check("プロジェクト直下の index.html が docs/index.html を指す",
      os.path.exists(os.path.join(ROOT, "index.html")) and
      "docs/index.html" in open(os.path.join(ROOT, "index.html"), encoding="utf-8").read())
if missing:
    print(f"\nRESULT: {ok} passed, {fail} failed")
    sys.exit(1)

pages = {p: read(p) for p in PAGES}
print()

# ---------------------------------------------------------------------------
print("== 2. 分量 ==")
for p in PAGES:
    size = os.path.getsize(os.path.join(DOCS, p))
    check(f"{p} が 57KB 以上（{size / 1024:.1f} KB）", size >= MIN_SIZE, f"{size} バイト")
print()

# ---------------------------------------------------------------------------
print("== 3. aside.spine が全11ページで完全一致するか（current の位置だけが違う） ==")
spine_re = re.compile(r'<aside class="spine">.*?</aside>', re.S)
norm = {}
for p in PAGES:
    m = spine_re.search(pages[p])
    if not m:
        check(f"{p} に aside.spine がある", False)
        continue
    s = m.group(0)
    # current の印だけを取り除いて比較する
    n = s.replace(' class="nav-item current" aria-current="page"', ' class="nav-item"')
    norm[p] = n

base = norm.get(PAGES[0])
for p in PAGES:
    check(f"{p} の spine が序章と一致する", norm.get(p) == base,
          "current 以外に差分があります")

# current がちょうど1つ、しかも自分のページを指しているか
for p in PAGES:
    m = spine_re.search(pages[p])
    s = m.group(0) if m else ""
    cur = re.findall(r'<a class="nav-item current" aria-current="page" href="([^"]+)"', s)
    check(f"{p} の current がちょうど1つで自分を指す",
          cur == [p], f"見つかった current: {cur}")
print()

# ---------------------------------------------------------------------------
print("== 4. pager の連結とリンク切れ ==")
for p in PAGES:
    prev, nxt = PAGER[p]
    foot = re.search(r'<div class="chap-foot">.*?</div>\s*</main>', pages[p], re.S)
    body = foot.group(0) if foot else ""
    check(f"{p} に chap-foot がある", bool(foot))
    if not foot:
        continue
    prev_found = re.findall(r'<a class="pager" href="([^"]+)"', body)
    next_found = re.findall(r'<a class="pager next" href="([^"]+)"', body)
    check(f"{p} の「前へ」が {prev}", prev_found == ([prev] if prev else []), f"実際: {prev_found}")
    check(f"{p} の「次へ」が {nxt}", next_found == ([nxt] if nxt else []), f"実際: {next_found}")

# サイト内リンクの参照先がすべて存在するか
broken = []
for p in PAGES:
    for href in re.findall(r'href="([^"#:]+\.html)(?:#[^"]*)?"', pages[p]):
        if not os.path.exists(os.path.join(DOCS, href)):
            broken.append((p, href))
check(f"サイト内リンクにリンク切れが無い（{len(broken)} 件）", not broken, f"{broken[:5]}")

# ページ内アンカーの参照先が存在するか
anchor_broken = []
for p in PAGES:
    ids = set(re.findall(r'\sid="([^"]+)"', pages[p]))
    for frag in re.findall(r'href="#([^"]+)"', pages[p]):
        if frag not in ids:
            anchor_broken.append((p, frag))
check(f"ページ内アンカーが全部存在する（{len(anchor_broken)} 件の切れ）", not anchor_broken, f"{anchor_broken[:5]}")
print()

# ---------------------------------------------------------------------------
print("== 5. 禁止事項 ==")
for p in PAGES:
    n = len(re.findall(r'\sstyle="', pages[p]))
    check(f"{p} にインライン style= が無い", n == 0, f"{n} 件")
total_script = sum(len(re.findall(r'<script', pages[p])) for p in PAGES)
check(f"全ページに <script> が無い（{total_script} 件）", total_script == 0)
for p in PAGES:
    check(f"{p} が style.css を読み込んでいる", 'href="style.css"' in pages[p])
print()

# ---------------------------------------------------------------------------
print("== 6. 掲載コードの関数名・型名が src/ に実在するか ==")
src_text = ""
for dirpath, _, files in os.walk(SRC):
    for f in files:
        if f.endswith(".go"):
            with open(os.path.join(dirpath, f), encoding="utf-8") as fh:
                src_text += fh.read()

# src/ に定義されている識別子（関数・型・定数・変数）
defined = set(re.findall(r'^func (?:\([^)]*\) )?([A-Za-z_][A-Za-z0-9_]*)', src_text, re.M))
defined |= set(re.findall(r'^type ([A-Za-z_][A-Za-z0-9_]*)', src_text, re.M))
defined |= set(re.findall(r'^(?:const|var) ([A-Za-z_][A-Za-z0-9_]*)', src_text, re.M))
defined |= set(re.findall(r'^\t([A-Za-z_][A-Za-z0-9_]*)\s+[=A-Za-z]', src_text, re.M))

# 教材の <span class="ty"> は「ユーザー定義型・エクスポートされた識別子」。
# 標準ライブラリのものが大半なので、mydocker 固有の名前だけを照合する。
OURS = {n for n in defined if n and n[0].isupper()}
missing_syms = {}
for p in PAGES:
    for sym in set(re.findall(r'<span class="ty">([A-Za-z_][A-Za-z0-9_]*)</span>', pages[p])):
        # mydocker のパッケージ名で修飾されているものだけを厳密に見る
        for pkg in ("container.", "cgroup.", "image.", "overlay.", "cli."):
            if f'{pkg}<span class="ty">{sym}</span>' in pages[p] and sym not in defined:
                missing_syms.setdefault(p, set()).add(pkg + sym)
check(f"教材が参照する mydocker の識別子がすべて src/ に実在する（不一致 {len(missing_syms)} ページ）",
      not missing_syms, f"{dict(list(missing_syms.items())[:3])}")

# 章が担当ソースのファイル名に言及しているか（file-tag）
EXPECT_FILES = {
    "00-env.html": ["docker-compose.yml", "go.mod"],
    "01-reexec.html": ["internal/container/run.go", "internal/container/init.go"],
    "02-namespace.html": ["internal/container/run.go"],
    "03-pivotroot.html": ["internal/container/init.go"],
    "04-cgroup.html": ["internal/cgroup/cgroup.go"],
    "05-registry.html": ["internal/image/registry.go"],
    "06-overlay.html": ["internal/overlay/overlay.go", "internal/image/extract.go"],
    "07-cli.html": ["internal/container/state.go", "cmd/mydocker/main.go"],
}
for p, files in EXPECT_FILES.items():
    for f in files:
        check(f"{p} が {f} を file-tag で示している", f in pages[p])
print()

# ---------------------------------------------------------------------------
print("== 7. 実装していないものを「できた」ように書いていないか ==")
# network.go は存在しない。教材がそれを掲載していたら「できたふり」である。
check("src/internal/network/network.go は存在しない（未実装の宣言どおり）",
      not os.path.exists(os.path.join(SRC, "internal", "network", "network.go")))
for p in PAGES:
    check(f"{p} が存在しない network.go を file-tag で掲載していない",
          'class="file-tag">internal/network/network.go' not in pages[p] and
          'class="file-tag">network/network.go' not in pages[p])
check("08章が未実装であることを明示している",
      "実装していません" in pages["08-network.html"] or
      "実装していない" in pages["08-network.html"] or
      "着手しなかった" in pages["08-network.html"])
print()

# ---------------------------------------------------------------------------
print("== 8. Go のビルドと vet ==")
for cmd in ("go build ./...", "go vet ./...", "gofmt -l ."):
    p = subprocess.run(cmd, shell=True, cwd=SRC, capture_output=True, text=True)
    if cmd.startswith("gofmt"):
        check(f"{cmd} の出力が空", p.stdout.strip() == "", f"{p.stdout.strip()!r}")
    else:
        check(f"{cmd} が通る", p.returncode == 0, f"{p.stderr.strip()[:200]!r}")
print()

print(f"RESULT: {ok} passed, {fail} failed")
sys.exit(1 if fail else 0)
