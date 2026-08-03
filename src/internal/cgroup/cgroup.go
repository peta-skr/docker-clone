// Package cgroup は cgroup による資源制限を扱う。
//
// cgroup は「ファイルシステム操作そのもの」である。ディレクトリを作り、ファイルに数値を書き、
// cgroup.procs に PID を書く。それだけで上限が効く。魔法は無い。
//
// ただし作法が cgroup v2 と v1 で違う。しかも「v2 がマウントされている」ことと
// 「v2 で memory が使える」ことは別である（疎通実験 00_smoke_test.md §3.3 で実際に踏んだ）。
// そこでこのパッケージは v2 を優先し、memory コントローラが v2 側に見えなければ v1 に落ちる。
package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// cgroup2 のスーパーブロックマジック。statfs の f_type がこの値なら cgroup v2 である。
// /sys/fs/cgroup が tmpfs のこともある（v1 と v2 が同居する hybrid 構成）ので、
// パスの存在ではなくファイルシステムの種類で判定する。
const cgroup2Magic = 0x63677270

// groupName は mydocker が作る親ディレクトリの名前。
// 実際のコンテナはこの下の <コンテナID> に作られる。
const groupName = "mydocker"

// Limits は利用者が指定した上限。ゼロ値は「上限なし」を意味する。
type Limits struct {
	MemoryBytes int64   // 例: 10485760（10MiB）
	CPUs        float64 // 例: 0.5（0.5 コア分）
	PidsMax     int64   // 例: 64
}

// IsZero は上限が1つも指定されていないことを返す。
func (l Limits) IsZero() bool {
	return l.MemoryBytes == 0 && l.CPUs == 0 && l.PidsMax == 0
}

// Cgroup は作成済みの cgroup 1つを表す。
type Cgroup struct {
	version string   // "v2" または "v1"
	dirs    []string // 実際に作ったディレクトリ（後片付けの対象）
	id      string
}

// Version は使ったバックエンドを返す（"v2" / "v1"）。
func (c *Cgroup) Version() string { return c.version }

// Dirs は作成した cgroup ディレクトリの一覧を返す。後片付けの検査に使う。
func (c *Cgroup) Dirs() []string { return append([]string(nil), c.dirs...) }

// ErrUnsupported はこの環境で cgroup による制限がかけられないことを表す。
// ⚠ 黙って無視してはいけない。上限を指定したのに効かないまま動くのが最悪の結果である。
var ErrUnsupported = errors.New("この環境では cgroup による資源制限が使えない（v2 に memory が無く、v1 の階層も見つからない）")

// New は id に対応する cgroup を作り、limits を書き込む。
// この時点ではまだプロセスは所属していない。Apply で入れる。
func New(id string, limits Limits) (*Cgroup, error) {
	if root, ok := findV2Root(); ok {
		return newV2(root, id, limits)
	}
	if roots, ok := findV1Roots(); ok {
		return newV1(roots, id, limits)
	}
	return nil, ErrUnsupported
}

// Apply は pid を cgroup に入れる。ここで初めて上限が効き始める。
//
// ⚠ 「作った」と「入れた」は別である。Apply を忘れた cgroup は
// memory.max に値が入っているのに1バイトも制限しない。
func (c *Cgroup) Apply(pid int) error {
	for _, dir := range c.dirs {
		procs := filepath.Join(dir, "cgroup.procs")
		if err := os.WriteFile(procs, []byte(strconv.Itoa(pid)), 0o644); err != nil {
			return fmt.Errorf("cgroup.procs への書き込みに失敗しました (%s): %w", procs, err)
		}
	}
	return nil
}

// Destroy は cgroup ディレクトリを片付ける。
//
// ⚠ cgroupfs は通常のファイルシステムではないので rm -rf では消えない。rmdir を使う。
// ⚠ 中にプロセスが残っていると rmdir は EBUSY で失敗する。これは正しい挙動なので、
// 握りつぶさずエラーとして返す。
func (c *Cgroup) Destroy() error {
	var firstErr error
	for _, dir := range c.dirs {
		if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
			if firstErr == nil {
				firstErr = fmt.Errorf("cgroup ディレクトリを削除できませんでした (%s): %w", dir, err)
			}
			continue
		}
		// 親（.../mydocker）も空になったら片付ける。
		// 他のコンテナが動いていれば ENOTEMPTY で失敗するが、それが正しいので無視してよい。
		_ = os.Remove(filepath.Dir(dir))
	}
	return firstErr
}

// ---------------------------------------------------------------------------
// cgroup v2
// ---------------------------------------------------------------------------

// findV2Root は「memory コントローラが実際に使える」cgroup v2 のルートを探す。
//
// ⚠ ここが疎通実験でいちばん効いた分岐である。
// cgroup2 がマウントされていても cgroup.controllers に memory が無いことがある。
// hybrid 構成では memory が v1 側に取られており、v2 からは決して使えない。
// 「マウントされているか」ではなく「コントローラが見えるか」で判定する。
func findV2Root() (string, bool) {
	candidates := []string{
		"/sys/fs/cgroup",
		"/sys/fs/cgroup/unified", // hybrid 構成ではここに v2 がぶら下がる
	}
	for _, root := range candidates {
		var st syscall.Statfs_t
		if err := syscall.Statfs(root, &st); err != nil {
			continue
		}
		if uint32(st.Type) != cgroup2Magic {
			continue
		}
		ctrls, err := os.ReadFile(filepath.Join(root, "cgroup.controllers"))
		if err != nil {
			continue
		}
		if !hasField(string(ctrls), "memory") {
			// v2 ではあるが memory が使えない。ここを掴むと memory.max が作られず静かに壊れる。
			continue
		}
		return root, true
	}
	return "", false
}

func newV2(root, id string, limits Limits) (*Cgroup, error) {
	parent := filepath.Join(root, groupName)
	leaf := filepath.Join(parent, id)

	// ⚠ 親の cgroup.subtree_control でコントローラを有効にしないと、
	//    子ディレクトリに memory.max などのファイルがそもそも作られない。
	if err := enableSubtree(root); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("cgroup の親ディレクトリを作れませんでした: %w", err)
	}
	if err := enableSubtree(parent); err != nil {
		_ = os.Remove(parent)
		return nil, err
	}
	// ⚠ cgroup v2 の「内部プロセス禁止」規則により、プロセスを入れられるのは葉だけである。
	//    parent には入れず、必ず leaf を作ってそこに入れる。
	if err := os.Mkdir(leaf, 0o755); err != nil {
		return nil, fmt.Errorf("cgroup ディレクトリを作れませんでした: %w", err)
	}

	c := &Cgroup{version: "v2", dirs: []string{leaf}, id: id}

	if limits.MemoryBytes > 0 {
		if err := write(leaf, "memory.max", strconv.FormatInt(limits.MemoryBytes, 10)); err != nil {
			c.Destroy()
			return nil, err
		}
		// スワップまで許すと「上限を超えても死なない」ので、スワップ側も同じ値で締める。
		// swap accounting が無い環境では失敗するが、それは致命ではない。
		_ = write(leaf, "memory.swap.max", "0")
	}
	if limits.CPUs > 0 {
		// cpu.max は "<quota> <period>"。period 100000us(=100ms) のうち何us使えるか。
		const period = 100000
		quota := int64(limits.CPUs * period)
		if err := write(leaf, "cpu.max", fmt.Sprintf("%d %d", quota, period)); err != nil {
			c.Destroy()
			return nil, err
		}
	}
	if limits.PidsMax > 0 {
		if err := write(leaf, "pids.max", strconv.FormatInt(limits.PidsMax, 10)); err != nil {
			c.Destroy()
			return nil, err
		}
	}
	return c, nil
}

// enableSubtree は dir の subtree_control に必要なコントローラを足す。
// 既に有効なものを足しても害はない。権限や構成の都合で一部が拒否されることがあるので、
// 1つずつ書いて「memory すら通らない」ときだけエラーにする。
func enableSubtree(dir string) error {
	path := filepath.Join(dir, "cgroup.subtree_control")
	cur, _ := os.ReadFile(path)
	var memErr error
	for _, ctrl := range []string{"memory", "cpu", "pids"} {
		if hasField(string(cur), ctrl) {
			continue
		}
		err := os.WriteFile(path, []byte("+"+ctrl), 0o644)
		if err != nil && ctrl == "memory" {
			memErr = fmt.Errorf("cgroup v2 で memory を有効にできませんでした (%s): %w", path, err)
		}
	}
	return memErr
}

// ---------------------------------------------------------------------------
// cgroup v1
// ---------------------------------------------------------------------------

// v1Controllers は v1 で使うコントローラと、そのマウント位置の候補。
var v1Controllers = []string{"memory", "cpu", "pids"}

// findV1Roots は v1 のコントローラごとのマウント位置を返す。
// memory が見つからなければ v1 は使えないと判断する（メモリ上限がこの教材の Must なので）。
func findV1Roots() (map[string]string, bool) {
	roots := map[string]string{}
	for _, ctrl := range v1Controllers {
		dir := filepath.Join("/sys/fs/cgroup", ctrl)
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			// cgroup v1 のディレクトリなら必ず cgroup.procs がある
			if _, err := os.Stat(filepath.Join(dir, "cgroup.procs")); err == nil {
				roots[ctrl] = dir
			}
		}
	}
	if _, ok := roots["memory"]; !ok {
		return nil, false
	}
	return roots, true
}

func newV1(roots map[string]string, id string, limits Limits) (*Cgroup, error) {
	c := &Cgroup{version: "v1", id: id}

	// v1 は「コントローラごとに別の階層」なので、使うコントローラの数だけディレクトリを作る。
	// v2 のような subtree_control は無い（そのぶん素直だが、後片付けの対象が増える）。
	mk := func(ctrl string) (string, bool, error) {
		root, ok := roots[ctrl]
		if !ok {
			return "", false, nil
		}
		dir := filepath.Join(root, groupName, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", false, fmt.Errorf("cgroup(v1) ディレクトリを作れませんでした (%s): %w", dir, err)
		}
		c.dirs = append(c.dirs, dir)
		return dir, true, nil
	}

	if limits.MemoryBytes > 0 {
		dir, ok, err := mk("memory")
		if err != nil {
			c.Destroy()
			return nil, err
		}
		if !ok {
			c.Destroy()
			return nil, errors.New("memory コントローラが見つからないため、メモリ上限をかけられません")
		}
		if err := write(dir, "memory.limit_in_bytes", strconv.FormatInt(limits.MemoryBytes, 10)); err != nil {
			c.Destroy()
			return nil, err
		}
		// スワップ込みの上限。swap accounting が無い環境では失敗するので、失敗は無視する。
		_ = write(dir, "memory.memsw.limit_in_bytes", strconv.FormatInt(limits.MemoryBytes, 10))
	}
	if limits.CPUs > 0 {
		dir, ok, err := mk("cpu")
		if err != nil {
			c.Destroy()
			return nil, err
		}
		if ok {
			const period = 100000
			if err := write(dir, "cpu.cfs_period_us", strconv.Itoa(period)); err != nil {
				c.Destroy()
				return nil, err
			}
			if err := write(dir, "cpu.cfs_quota_us", strconv.FormatInt(int64(limits.CPUs*period), 10)); err != nil {
				c.Destroy()
				return nil, err
			}
		}
	}
	if limits.PidsMax > 0 {
		dir, ok, err := mk("pids")
		if err != nil {
			c.Destroy()
			return nil, err
		}
		if ok {
			if err := write(dir, "pids.max", strconv.FormatInt(limits.PidsMax, 10)); err != nil {
				c.Destroy()
				return nil, err
			}
		}
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// 小道具
// ---------------------------------------------------------------------------

func write(dir, name, value string) error {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		return fmt.Errorf("%s に %q を書けませんでした: %w", path, value, err)
	}
	return nil
}

// hasField は空白区切りの一覧に word が含まれるかを返す。
// strings.Contains では "memory" が "memory_foo" に誤ヒットするので、必ず語単位で見る。
func hasField(list, word string) bool {
	for _, f := range strings.Fields(list) {
		if f == word {
			return true
		}
	}
	return false
}

// ParseMemory は "10m" / "512k" / "1g" / "1048576" を解釈してバイト数を返す。
func ParseMemory(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "k"), strings.HasSuffix(s, "kb"):
		mult, s = 1<<10, strings.TrimSuffix(strings.TrimSuffix(s, "b"), "k")
	case strings.HasSuffix(s, "m"), strings.HasSuffix(s, "mb"):
		mult, s = 1<<20, strings.TrimSuffix(strings.TrimSuffix(s, "b"), "m")
	case strings.HasSuffix(s, "g"), strings.HasSuffix(s, "gb"):
		mult, s = 1<<30, strings.TrimSuffix(strings.TrimSuffix(s, "b"), "g")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("メモリ量として解釈できません: %q（例: 10m, 512k, 1g）", s)
	}
	if n <= 0 {
		return 0, fmt.Errorf("メモリ量は正の数で指定してください: %q", s)
	}
	return n * mult, nil
}

// Describe は現在の環境で使えるバックエンドを人間向けに説明する。
// 「なぜ効かないのか」を利用者に伝えるために使う。
func Describe() string {
	if root, ok := findV2Root(); ok {
		return fmt.Sprintf("cgroup v2 (%s)", root)
	}
	if roots, ok := findV1Roots(); ok {
		var names []string
		for _, ctrl := range v1Controllers {
			if _, ok := roots[ctrl]; ok {
				names = append(names, ctrl)
			}
		}
		return fmt.Sprintf("cgroup v1 (/sys/fs/cgroup, 利用可能: %s)", strings.Join(names, " "))
	}
	return "利用不可"
}
