package container

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Root は mydocker が状態を置く場所。
//
//	<Root>/containers/<ID>/state.json … 実行中コンテナの記録
//	<Root>/containers/<ID>/merged     … overlay の結果（＝コンテナの /）
//	<Root>/containers/<ID>/upper      … 書き込みはここに溜まる
//	<Root>/layers/<digest>/           … 展開済みのイメージレイヤ（共有・読み取り専用）
const Root = "/var/lib/mydocker"

// State は1つのコンテナの記録。ps と exec と後片付けが、これだけを頼りに動く。
type State struct {
	ID        string    `json:"id"`
	Image     string    `json:"image"`
	Command   []string  `json:"command"`
	PID       int       `json:"pid"`
	Hostname  string    `json:"hostname"`
	Workdir   string    `json:"workdir"`
	CreatedAt time.Time `json:"created_at"`

	Merged        string   `json:"merged"`         // overlay の merged（空なら overlay を使っていない）
	CgroupDirs    []string `json:"cgroup_dirs"`    // 後片付けの対象
	CgroupVersion string   `json:"cgroup_version"` // "v2" / "v1" / ""

	// ⚠ PID は再利用される。終了したコンテナの PID が別のプロセスに割り当てられると、
	//    「まだ生きている」と誤判定する。そこで起動時刻（/proc/<pid>/stat の 22 番目）を
	//    一緒に記録し、両方が一致したときだけ生存とみなす。
	StartTime uint64 `json:"start_time"`
}

// NewID は 12 桁のコンテナ ID を作る。docker の短い ID と同じ見た目にしてある。
func NewID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

func containersDir() string      { return filepath.Join(Root, "containers") }
func stateDir(id string) string  { return filepath.Join(containersDir(), id) }
func statePath(id string) string { return filepath.Join(stateDir(id), "state.json") }

// Save は状態を書き出す。
func (s *State) Save() error {
	if err := os.MkdirAll(stateDir(s.ID), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(statePath(s.ID), b, 0o644)
}

// Load は ID から状態を読む。前方一致でも引ける（docker と同じ使い勝手）。
func Load(id string) (*State, error) {
	if s, err := loadExact(id); err == nil {
		return s, nil
	}
	// 前方一致
	entries, err := os.ReadDir(containersDir())
	if err != nil {
		return nil, fmt.Errorf("コンテナが見つかりません: %s", id)
	}
	var hit []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), id) {
			hit = append(hit, e.Name())
		}
	}
	switch len(hit) {
	case 0:
		return nil, fmt.Errorf("コンテナが見つかりません: %s", id)
	case 1:
		return loadExact(hit[0])
	default:
		return nil, fmt.Errorf("ID %s は %d 件に一致します。もう少し長く指定してください", id, len(hit))
	}
}

func loadExact(id string) (*State, error) {
	b, err := os.ReadFile(statePath(id))
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s の状態ファイルが壊れています: %w", id, err)
	}
	return &s, nil
}

// RemoveRecord は状態ファイルとコンテナ用ディレクトリを消す。
//
// ⚠ 呼び出す前に必ずアンマウントを済ませ、**成功を確認してから**呼ぶこと。
//
//	マウントされたまま消すと、マウント越しに lowerdir（＝イメージの実体）まで消える。
func RemoveRecord(s *State) error {
	if s.Merged != "" && isMounted(s.Merged) {
		return fmt.Errorf("まだマウントされているため削除しません: %s", s.Merged)
	}
	return os.RemoveAll(stateDir(s.ID))
}

// List は実行中のコンテナだけを返す。
//
// 死んだのに記録が残っているもの（mydocker 自身が強制終了された場合など）は
// 一覧には出さないが、**勝手に消しもしない**。マウントが残っている可能性があり、
// 確認せずに消すのが最も危険だからである。片付けは mydocker rm で明示的に行う。
func List() ([]*State, error) {
	entries, err := os.ReadDir(containersDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var alive []*State
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s, err := loadExact(e.Name())
		if err != nil {
			continue
		}
		if s.IsAlive() {
			alive = append(alive, s)
		}
	}
	sort.Slice(alive, func(i, j int) bool { return alive[i].CreatedAt.Before(alive[j].CreatedAt) })
	return alive, nil
}

// IsAlive はそのコンテナの PID 1 がまだ生きているかを返す。
//
// ⚠ /proc/<pid> の存在だけでは足りない。PID は再利用されるので、
//
//	起動時刻まで一致してはじめて「同じプロセス」と言える。
func (s *State) IsAlive() bool {
	if s.PID <= 0 {
		return false
	}
	st, err := readProcStat(s.PID)
	if err != nil {
		return false
	}
	if st.state == "Z" { // ゾンビは死んでいる扱い
		return false
	}
	if s.StartTime != 0 && st.startTime != s.StartTime {
		return false // PID が再利用された別のプロセス
	}
	return true
}

type procStat struct {
	state     string
	startTime uint64
}

// readProcStat は /proc/<pid>/stat から状態と起動時刻を読む。
//
// この行の 2 番目のフィールドはコマンド名で、括弧に包まれている。
// ⚠ コマンド名に空白や括弧が入りうるので、素朴に空白で split すると位置がずれる。
// 最後の ")" を探してからその先を切るのが定石。
func readProcStat(pid int) (procStat, error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return procStat{}, err
	}
	line := string(b)
	i := strings.LastIndex(line, ")")
	if i < 0 || i+2 >= len(line) {
		return procStat{}, fmt.Errorf("/proc/%d/stat を解釈できません", pid)
	}
	fields := strings.Fields(line[i+2:])
	// ここでの fields[0] は元の 3 番目のフィールド（state）に当たる。
	// 起動時刻は元の 22 番目なので fields[19]。
	if len(fields) < 20 {
		return procStat{}, fmt.Errorf("/proc/%d/stat の項目が足りません", pid)
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return procStat{}, err
	}
	return procStat{state: fields[0], startTime: start}, nil
}

// ReadStartTime は起動直後の子プロセスの起動時刻を読む（State に記録するため）。
func ReadStartTime(pid int) uint64 {
	st, err := readProcStat(pid)
	if err != nil {
		return 0
	}
	return st.startTime
}

// isMounted はマウントポイントかどうかを親との st_dev 比較で判定する。
// 別のファイルシステムが被さっていれば、自分と親でデバイス番号が変わる。
func isMounted(path string) bool {
	var st, parent syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return false
	}
	if err := syscall.Lstat(filepath.Dir(path), &parent); err != nil {
		return false
	}
	return st.Dev != parent.Dev
}
