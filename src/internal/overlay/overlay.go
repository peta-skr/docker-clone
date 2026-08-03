// Package overlay は OverlayFS で複数のレイヤを1つのファイルシステムに重ね合わせる。
//
// コンテナの「使い捨て」の正体はここにある。
// 読み取り専用の下層（lowerdir）はイメージそのままで、書き込みは全部 upperdir に溜まる。
// コンテナを消すときは upperdir を捨てるだけでよい。イメージには指一本触れていない。
package overlay

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Mount は1つの overlay マウントを表す。
type Mount struct {
	Merged string   // 重ね合わせた結果が見えるディレクトリ（＝コンテナの /）
	Upper  string   // 書き込み先。コンテナを消すときはここを捨てる
	Work   string   // overlayfs が内部で使う作業場所
	Lowers []string // 下から上の順（先頭が最下層）
}

// Supported はこの環境で overlayfs が使えるかを返す。
// /proc/filesystems に overlay が並んでいるかで判定する。
func Supported() bool {
	b, err := os.ReadFile("/proc/filesystems")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "nodev\toverlay" || strings.HasSuffix(strings.TrimSpace(line), "\toverlay") ||
			strings.TrimSpace(line) == "overlay" {
			return true
		}
	}
	return strings.Contains(string(b), "overlay")
}

// New は base の下に merged / upper / work を用意し、lowers を重ねてマウントする。
// lowers は **下から上の順**（先頭が最下層）で渡すこと。イメージのレイヤ順そのままでよい。
func New(base string, lowers []string) (*Mount, error) {
	if len(lowers) == 0 {
		return nil, fmt.Errorf("重ねる下層が1つもありません")
	}
	for _, l := range lowers {
		if fi, err := os.Stat(l); err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("下層が見つかりません: %s", l)
		}
		// ⚠ lowerdir のオプション文字列は ":" 区切り、"," でオプションが切れる。
		//    パスにこれらが混ざると文字列が壊れる。digest 由来のパスなら起きないが、念のため弾く。
		if strings.ContainsAny(l, ":,") {
			return nil, fmt.Errorf("下層のパスに : か , が含まれています: %s", l)
		}
	}

	m := &Mount{
		Merged: filepath.Join(base, "merged"),
		Upper:  filepath.Join(base, "upper"),
		Work:   filepath.Join(base, "work"),
		Lowers: append([]string(nil), lowers...),
	}
	for _, d := range []string{m.Merged, m.Upper, m.Work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	// ⚠ workdir は空でなければならない。前回の残骸があると EINVAL で落ちる。
	//    ⚠ upperdir と同じファイルシステム上に無いと EXDEV / EINVAL になる。
	//      ここでは同じ base の下に作っているので条件を満たす。
	if err := emptyDir(m.Work); err != nil {
		return nil, err
	}

	// ⚠ lowerdir は **左が上（優先）**。
	//    イメージのレイヤは「後のものほど上」なので、並べる順を逆にする必要がある。
	//    ここを間違えると、古い層が新しい層を隠して「更新が反映されていない」ように見える。
	rev := make([]string, 0, len(lowers))
	for i := len(lowers) - 1; i >= 0; i-- {
		rev = append(rev, lowers[i])
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
		strings.Join(rev, ":"), m.Upper, m.Work)

	if err := syscall.Mount("overlay", m.Merged, "overlay", 0, opts); err != nil {
		return nil, fmt.Errorf("overlay のマウントに失敗しました (%s): %w", opts, err)
	}
	return m, nil
}

// Unmount は重ね合わせを外す。
//
// ⚠ umount は EBUSY（デバイスが使用中）で日常的に失敗する。
// コンテナのプロセスが残っていたり、シェルの作業ディレクトリがその中だったりするだけで起きる。
// ここでエラーを握りつぶして呼び出し元が rm -rf に進むと、
// **マウント越しに lowerdir の実体（＝イメージの中身）まで消える。**
// だから失敗は必ずエラーとして返す。呼び出し元は削除に進んではいけない。
func (m *Mount) Unmount() error {
	if err := syscall.Unmount(m.Merged, 0); err != nil {
		// MNT_DETACH は「今すぐ切り離し、参照が切れたら実際に外す」遅延アンマウント。
		// EBUSY のときの最後の手段として一度だけ試す。
		if err2 := syscall.Unmount(m.Merged, syscall.MNT_DETACH); err2 != nil {
			return fmt.Errorf("overlay を外せませんでした (%s): %w", m.Merged, err)
		}
	}
	// ★本当に外れたかを確認してからでなければ、呼び出し元は削除に進んではいけない。
	if IsMountPoint(m.Merged) {
		return fmt.Errorf("overlay がまだマウントされたままです: %s", m.Merged)
	}
	return nil
}

// IsMountPoint はパスがマウントポイントかを返す。
// 自分の st_dev と親ディレクトリの st_dev を比べる（mountpoint(1) と同じ考え方）。
func IsMountPoint(path string) bool {
	var st, parent syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return false
	}
	if err := syscall.Lstat(filepath.Dir(path), &parent); err != nil {
		return false
	}
	return st.Dev != parent.Dev
}

// CountMounts は /proc/mounts に現れる、指定の種類のマウント数を数える。
// 「10回起動して10個残っていないか」を数えて確かめるために使う。
func CountMounts(fstype string) (int, error) {
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[2] == fstype {
			n++
		}
	}
	return n, nil
}

func emptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}
