package container

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// syncFD は親から「準備ができた」合図を受け取るためのファイルディスクリプタ。
//
// なぜ必要か: 親は子を起動してから cgroup に PID を入れる。その間に子が走り出すと、
// **上限が効く前にメモリを確保してしまう**。それでは「上限を超えたら死ぬ」テストが
// 気まぐれに通ったり落ちたりする。そこで子はここで一度止まり、親の合図を待つ。
const syncFD = 3

// syncReady は親が送る合図。これ以外が来たら（＝親が失敗したら）子は何もせず終わる。
const syncReady = "OK"

// oldRoot は pivot_root で押し出された古い root の一時的な置き場。
const oldRoot = "/.old_root"

// InitArgs は init サブコマンドが受け取る引数。
//
//	mydocker init <rootfs> <hostname> <workdir> <cmd> [args...]
//
// ★この init は利用者が直接叩くものではない。
// 親プロセスが /proc/self/exe を使って自分自身を「別の顔」で起動するためだけに存在する
// （re-exec パターン）。
type InitArgs struct {
	Rootfs   string
	Hostname string
	Workdir  string
	Argv     []string
	Env      []string
}

// Init は新しい名前空間の中（＝コンテナの PID 1）で走る処理である。
//
// ここに来た時点で、親が Cloneflags で作った namespace の中にいる。
// あとは「地面を入れ替えて、目的のコマンドに成り代わる」だけ。
//
//  1. 親の合図を待つ（cgroup に入れてもらうまで動かない）
//  2. ホスト名を変える（UTS 名前空間の効果を目に見える形にする）
//  3. pivot_root で root を挿げ替える
//  4. /proc /sys /dev を貼り直す
//  5. syscall.Exec で目的のコマンドに成り代わる（★戻ってこない）
func Init(args InitArgs) error {
	if err := waitForParent(); err != nil {
		return err
	}

	if args.Hostname != "" {
		if err := syscall.Sethostname([]byte(args.Hostname)); err != nil {
			return fmt.Errorf("ホスト名を設定できませんでした: %w", err)
		}
	}

	if err := pivotRoot(args.Rootfs); err != nil {
		return err
	}
	if err := mountSpecialFilesystems(); err != nil {
		return err
	}

	// 実行するコマンドを新しい root の中から探す。
	// ⚠ ここは pivot_root の **後** でなければならない。前に探すとホスト側の
	//    /bin/sh を掴んでしまい、「コンテナの中なのにホストのバイナリが動く」ことになる。
	env := args.Env
	if len(env) == 0 {
		env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root", "TERM=xterm"}
	}
	if err := os.Setenv("PATH", envValue(env, "PATH")); err != nil {
		return err
	}

	// イメージが WorkingDir を指定していればそこへ移る。
	// 無ければ / のまま。存在しないディレクトリを指していても起動は止めない。
	if args.Workdir != "" && args.Workdir != "/" {
		if err := syscall.Chdir(args.Workdir); err != nil {
			fmt.Fprintf(os.Stderr, "警告: 作業ディレクトリ %s へ移れませんでした: %v\n", args.Workdir, err)
		}
	}
	path, err := exec.LookPath(args.Argv[0])
	if err != nil {
		return fmt.Errorf("コマンドが見つかりません: %s", args.Argv[0])
	}

	// ★syscall.Exec はプロセスを置き換える。成功すればこの行から先は実行されない。
	//   fork ではないので、コンテナの PID 1 はこのまま目的のコマンドそのものになる。
	if err := syscall.Exec(path, args.Argv, env); err != nil {
		return fmt.Errorf("%s の実行に失敗しました: %w", path, err)
	}
	return nil // 到達しない
}

// waitForParent は fd 3 から親の合図を読む。
// 親が合図を送らずに閉じた（＝準備に失敗した）場合はエラーにする。
func waitForParent() error {
	f := os.NewFile(uintptr(syncFD), "sync")
	if f == nil {
		return fmt.Errorf("親との同期用ディスクリプタがありません（init は直接実行できません）")
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("親からの合図を読めませんでした: %w", err)
	}
	if strings.TrimSpace(string(b)) != syncReady {
		return fmt.Errorf("親の準備が失敗したため中止します")
	}
	return nil
}

// pivotRoot は rootfs をコンテナの新しい / にする。
//
// ⚠ この順序を崩すと必ず失敗する。1つずつ理由がある。
//
// chroot ではなく pivot_root を使うのは、chroot には抜け出す既知の手口があり、
// 古い root のマウントも残るため。pivot_root は古い root を完全に切り離せる。
func pivotRoot(rootfs string) error {
	// 1. マウントの伝播をホストから切る。
	//    これをやらないと、この後の bind mount や /proc のマウントがホスト側に漏れる。
	if err := syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("マウントの伝播を切れませんでした: %w", err)
	}

	// 2. ⚠ 新しい root は「マウントポイント」でなければならない。
	//    普通のディレクトリを渡すと pivot_root は EINVAL で落ちる。
	//    自分自身に bind mount することでマウントポイントの条件を満たす。
	if err := syscall.Mount(rootfs, rootfs, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("rootfs を自分自身に bind mount できませんでした (%s): %w", rootfs, err)
	}

	// 3. 古い root の置き場を新しい root の中に用意する。
	//    ⚠ put_old は new_root の配下でなければならない。これも規則。
	putOld := filepath.Join(rootfs, oldRoot)
	if err := os.MkdirAll(putOld, 0o700); err != nil {
		return fmt.Errorf("古い root の置き場を作れませんでした: %w", err)
	}

	// 4. 根の挿げ替え。ここで / が入れ替わる。
	if err := syscall.PivotRoot(rootfs, putOld); err != nil {
		return fmt.Errorf("pivot_root に失敗しました: %w", err)
	}

	// 5. 作業ディレクトリは古い root を指したままなので、新しい / に移る。
	if err := syscall.Chdir("/"); err != nil {
		return fmt.Errorf("新しい / へ移動できませんでした: %w", err)
	}
	return nil
}

// dropOldRoot は古い root を切り離して捨てる。
// ⚠ /proc を貼り直した後に呼ぶこと。順序を逆にすると /proc のマウントで古い root を掴む。
func dropOldRoot() error {
	// MNT_DETACH は遅延アンマウント。まだ参照が残っていても切り離せる。
	if err := syscall.Unmount(oldRoot, syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("古い root を切り離せませんでした: %w", err)
	}
	if err := os.Remove(oldRoot); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("古い root の置き場を消せませんでした: %w", err)
	}
	return nil
}

// mountSpecialFilesystems は /proc /sys /dev を新しい root に貼り直す。
//
// ⚠ /proc を貼り直さないと PID 名前空間が効いていないように見える。
// 実際には効いているのに、/proc がホストのものを指したままなので
// ps がホストの全プロセスを出す。「namespace が壊れている」と誤診する典型である。
func mountSpecialFilesystems() error {
	// /proc は必須。ps が動くための最低条件。
	if err := os.MkdirAll("/proc", 0o555); err != nil {
		return err
	}
	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		return fmt.Errorf("/proc をマウントできませんでした: %w", err)
	}

	// ここまで来れば古い root はもう要らない。
	// ⚠ /proc より先に捨てると、/proc のマウントが古い root を掴んで失敗することがある。
	if err := dropOldRoot(); err != nil {
		return err
	}

	// /sys は無くても ps は動く。失敗しても致命ではないので警告に留める。
	if err := os.MkdirAll("/sys", 0o555); err == nil {
		if err := syscall.Mount("sysfs", "/sys", "sysfs",
			syscall.MS_NOSUID|syscall.MS_NOEXEC|syscall.MS_NODEV|syscall.MS_RDONLY, ""); err != nil {
			fmt.Fprintf(os.Stderr, "警告: /sys をマウントできませんでした: %v\n", err)
		}
	}

	// /dev は tmpfs を敷いて、必要な最小限のデバイスノードだけを自分で作る。
	// ホストの /dev をそのまま見せるとコンテナからホストのディスクが触れてしまう。
	if err := mountDev(); err != nil {
		fmt.Fprintf(os.Stderr, "警告: /dev を用意できませんでした: %v\n", err)
	}
	return nil
}

// devNode は最小限のデバイスノード定義。
type devNode struct {
	path  string
	mode  uint32
	major uint32
	minor uint32
}

func mountDev() error {
	if err := os.MkdirAll("/dev", 0o755); err != nil {
		return err
	}
	if err := syscall.Mount("tmpfs", "/dev", "tmpfs", syscall.MS_NOSUID, "mode=755,size=64k"); err != nil {
		return err
	}
	nodes := []devNode{
		{"/dev/null", syscall.S_IFCHR | 0o666, 1, 3},
		{"/dev/zero", syscall.S_IFCHR | 0o666, 1, 5},
		{"/dev/full", syscall.S_IFCHR | 0o666, 1, 7},
		{"/dev/random", syscall.S_IFCHR | 0o666, 1, 8},
		{"/dev/urandom", syscall.S_IFCHR | 0o666, 1, 9},
		{"/dev/tty", syscall.S_IFCHR | 0o666, 5, 0},
	}
	for _, n := range nodes {
		if err := syscall.Mknod(n.path, n.mode, int(mkdev(n.major, n.minor))); err != nil {
			fmt.Fprintf(os.Stderr, "警告: %s を作れませんでした: %v\n", n.path, err)
		}
	}
	// 標準入出力へのシンボリックリンク。シェルが期待することがある。
	_ = os.Symlink("/proc/self/fd", "/dev/fd")
	_ = os.Symlink("/proc/self/fd/0", "/dev/stdin")
	_ = os.Symlink("/proc/self/fd/1", "/dev/stdout")
	_ = os.Symlink("/proc/self/fd/2", "/dev/stderr")
	return nil
}

func mkdev(major, minor uint32) uint64 {
	return uint64(minor&0xff) | (uint64(major&0xfff) << 8) |
		((uint64(minor) &^ 0xff) << 12) | ((uint64(major) &^ 0xfff) << 32)
}

func envValue(env []string, key string) string {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return strings.TrimPrefix(e, prefix)
		}
	}
	return ""
}
