package container

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"mydocker/internal/cgroup"
	"mydocker/internal/overlay"
)

// Config は run に渡す設定。
type Config struct {
	Image    string   // 表示用のイメージ名（--rootfs 指定時は "(local)"）
	Lowers   []string // 重ねるレイヤ。★下から上の順
	Argv     []string // コンテナの中で実行するコマンド
	Env      []string // コンテナに渡す環境変数
	Hostname string
	Workdir  string // コンテナの中での作業ディレクトリ（イメージの WorkingDir）
	Limits   cgroup.Limits
	Quiet    bool // 進捗を出さない
}

// Run はコンテナを1つ起動し、終了まで待って終了コードを返す。
//
// ★この教材でいちばん説明の要る箇所が、次の re-exec パターンである。
//
// 素朴に考えると「unshare して、そのあと目的のコマンドを exec すればいい」と思える。ところが:
//
//   - PID 名前空間は unshare を呼んだプロセス自身には適用されない。
//     適用されるのは **その後に作られた子プロセス** から。「自分が PID 1 になる」ことはできない
//   - Go のランタイムはマルチスレッドなので、プロセス内でスレッド単位の unshare を素直に扱えない
//
// そこで /proc/self/exe（＝自分自身の実行ファイル）を "init" という別の顔で起動する。
// 新しい名前空間はこの起動と同時に生まれ、子は生まれながらにして PID 1 になる。
func Run(cfg Config) (int, error) {
	id := NewID()
	base := stateDir(id)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return 1, err
	}

	// --- 1. 根になるファイルシステムを用意する（レイヤの重ね合わせ）---
	mnt, err := overlay.New(base, cfg.Lowers)
	if err != nil {
		_ = os.RemoveAll(base)
		return 1, err
	}

	st := &State{
		ID:        id,
		Image:     cfg.Image,
		Command:   cfg.Argv,
		Hostname:  cfg.Hostname,
		Workdir:   cfg.Workdir,
		CreatedAt: time.Now(),
		Merged:    mnt.Merged,
	}

	// 後片付けは1か所にまとめる。途中でどこで失敗しても必ずここを通す。
	cleanup := func(cg *cgroup.Cgroup) error {
		var firstErr error
		if cg != nil {
			if err := cg.Destroy(); err != nil {
				firstErr = err
			}
		}
		// ⚠ アンマウントの成否を確かめずに削除へ進んではいけない。
		//    失敗したまま RemoveAll すると、マウント越しに lowerdir（＝イメージの実体）まで消える。
		if err := mnt.Unmount(); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			// ★削除には進まない。状態ファイルも残して、人間が調べられるようにする。
			return firstErr
		}
		if err := RemoveRecord(st); err != nil && firstErr == nil {
			firstErr = err
		}
		return firstErr
	}

	// --- 2. 資源の上限を用意する（プロセスはまだ入れない）---
	var cg *cgroup.Cgroup
	if !cfg.Limits.IsZero() {
		cg, err = cgroup.New(id, cfg.Limits)
		if err != nil {
			// ⚠ 上限を指定したのに黙って無視して走らせるのが最悪の結果。必ず止める。
			_ = cleanup(nil)
			return 1, err
		}
		st.CgroupDirs = cg.Dirs()
		st.CgroupVersion = cg.Version()
	}

	// --- 3. 子と同期するためのパイプ ---
	// 親が cgroup に PID を入れ終えるまで、子は走り出してはいけない。
	syncR, syncW, err := os.Pipe()
	if err != nil {
		_ = cleanup(cg)
		return 1, err
	}

	// --- 4. 自分自身を "init" という別の顔で起動する ---
	args := append([]string{"init", mnt.Merged, cfg.Hostname, orSlash(cfg.Workdir)}, cfg.Argv...)
	cmd := exec.Command("/proc/self/exe", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUTS | // ホスト名
			syscall.CLONE_NEWPID | // プロセス番号
			syscall.CLONE_NEWNS | // マウント
			syscall.CLONE_NEWIPC, // プロセス間通信
		// ⚠ これを忘れると、コンテナ内で行ったマウントがホスト側にも伝播する。
		//    pivot_root が EINVAL で落ちる原因にもなる。「なぜか動かない」の筆頭。
		Unshareflags: syscall.CLONE_NEWNS,
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = cfg.Env
	cmd.ExtraFiles = []*os.File{syncR} // 子から見て fd 3

	if err := cmd.Start(); err != nil {
		syncR.Close()
		syncW.Close()
		_ = cleanup(cg)
		return 1, fmt.Errorf("コンテナの起動に失敗しました: %w", err)
	}
	syncR.Close() // 親側では使わない

	pid := cmd.Process.Pid
	st.PID = pid
	st.StartTime = ReadStartTime(pid)

	// --- 5. cgroup に入れてから、子に合図を送る ---
	startErr := func() error {
		if cg != nil {
			if err := cg.Apply(pid); err != nil {
				return err
			}
		}
		return st.Save()
	}()
	if startErr != nil {
		syncW.Close() // 合図を送らずに閉じる → 子は何もせず終了する
		_ = cmd.Wait()
		_ = cleanup(cg)
		return 1, startErr
	}
	if _, err := syncW.WriteString(syncReady); err != nil {
		syncW.Close()
		_ = cmd.Wait()
		_ = cleanup(cg)
		return 1, err
	}
	syncW.Close()

	if !cfg.Quiet {
		fmt.Fprintf(os.Stderr, "コンテナ %s を起動しました (PID %d)\n", id, pid)
	}

	// --- 6. 終了を待つ ---
	// Ctrl-C は端末から子にも届く。親がここで死ぬと後片付けが走らないので、
	// 親は明示的にシグナルを受け流し、子の終了を待ってから片付ける。
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	go func() {
		for range sig {
		}
	}()

	waitErr := cmd.Wait()
	code := exitCode(waitErr)

	// --- 7. 後片付け（★ここを落とすと静かに壊れる）---
	if err := cleanup(cg); err != nil {
		return code, err
	}
	return code, nil
}

// Exec は実行中のコンテナの中でもう1つコマンドを走らせる。
//
// 実現の仕方:
//   - PID / UTS / IPC 名前空間は setns で **既存のコンテナのものに参加する**
//   - マウント名前空間だけは新しく作り、同じ merged ディレクトリを根にする
//
// なぜマウントだけ別扱いなのか: setns でマウント名前空間に入るには
// **呼び出し元がシングルスレッドでなければならない**（多スレッドだと EINVAL）。
// Go のランタイムは常にマルチスレッドなので、cgo を使わずにこれは満たせない。
// 同じ overlay の merged を根にすれば、見えるファイルは完全に同一なので実害は無い。
// （この割り切りは 03_implementation.md に判断として記録してある）
func Exec(st *State, argv []string, env []string) (int, error) {
	if !st.IsAlive() {
		return 1, fmt.Errorf("コンテナ %s は実行中ではありません", st.ID)
	}

	// ⚠ setns はスレッド単位の操作である。fork もこのスレッドから行われる必要があるので、
	//    goroutine を OS スレッドに固定してから触る。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// pid 名前空間への setns は「呼び出し元自身」ではなく「これから作る子」に効く。
	// つまりこの後の cmd.Start() で生まれる子が、コンテナの PID 名前空間に入る。
	for _, ns := range []struct {
		name string
		flag int
	}{
		{"uts", syscall.CLONE_NEWUTS},
		{"ipc", syscall.CLONE_NEWIPC},
		{"pid", syscall.CLONE_NEWPID},
	} {
		path := filepath.Join("/proc", fmt.Sprint(st.PID), "ns", ns.name)
		f, err := os.Open(path)
		if err != nil {
			return 1, fmt.Errorf("%s を開けませんでした: %w", path, err)
		}
		_, _, errno := syscall.Syscall(sysSetns, f.Fd(), uintptr(ns.flag), 0)
		f.Close()
		if errno != 0 {
			return 1, fmt.Errorf("%s 名前空間に参加できませんでした: %v", ns.name, errno)
		}
	}

	syncR, syncW, err := os.Pipe()
	if err != nil {
		return 1, err
	}
	args := append([]string{"init", st.Merged, st.Hostname, orSlash(st.Workdir)}, argv...)
	cmd := exec.Command("/proc/self/exe", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:   syscall.CLONE_NEWNS,
		Unshareflags: syscall.CLONE_NEWNS,
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = env
	cmd.ExtraFiles = []*os.File{syncR}

	if err := cmd.Start(); err != nil {
		syncR.Close()
		syncW.Close()
		return 1, err
	}
	syncR.Close()
	// exec では cgroup を触らないので、すぐに合図を送ってよい。
	_, _ = syncW.WriteString(syncReady)
	syncW.Close()

	return exitCode(cmd.Wait()), nil
}

// Cleanup は1つのコンテナの痕跡を片付ける。rm コマンドと、異常終了の後始末に使う。
//
// ⚠ 順序が命。「アンマウント → 削除」であり、**前者が失敗したら後者に進まない**。
func Cleanup(st *State) error {
	if st.IsAlive() {
		return fmt.Errorf("コンテナ %s はまだ実行中です。先に終了させてください", st.ID)
	}
	var firstErr error

	for _, dir := range st.CgroupDirs {
		if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
			firstErr = fmt.Errorf("cgroup ディレクトリを削除できませんでした (%s): %w", dir, err)
		}
	}
	if st.Merged != "" && overlay.IsMountPoint(st.Merged) {
		m := &overlay.Mount{Merged: st.Merged}
		if err := m.Unmount(); err != nil {
			// ★アンマウントに失敗したら絶対に削除へ進まない
			if firstErr == nil {
				firstErr = err
			}
			return firstErr
		}
	}
	if err := RemoveRecord(st); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// exitCode は子プロセスの終了状況をシェルの終了コードに翻訳する。
//
// ⚠ シグナルで殺されたプロセスの ExitCode() は -1 を返す。これをそのまま os.Exit に渡すと
// 255 になり、「OOM で殺された」ことが利用者に伝わらない。
// シェルの慣習に合わせて 128+シグナル番号 を返す（OOM kill なら SIGKILL(9) で 137）。
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return 1
}

// orSlash は空文字を "/" に読み替える。init へ位置引数で渡すため、空のままにできない。
func orSlash(s string) string {
	if s == "" {
		return "/"
	}
	return s
}

// LayerDirsExist は与えられたレイヤディレクトリがすべて存在するかを確かめる。
func LayerDirsExist(dirs []string) error {
	for _, d := range dirs {
		fi, err := os.Stat(d)
		if err != nil || !fi.IsDir() {
			return fmt.Errorf("レイヤのディレクトリがありません: %s", d)
		}
	}
	return nil
}
