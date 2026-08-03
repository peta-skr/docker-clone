// Package cli はサブコマンドの振り分けを行う。
//
// ここは薄く保つ。実際の仕事は internal/container・internal/cgroup・internal/image が持つ。
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"mydocker/internal/cgroup"
	"mydocker/internal/container"
	"mydocker/internal/image"
	"mydocker/internal/overlay"
)

// ExitError は「メッセージは既に出したので、この終了コードで終わってほしい」を表す。
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

const usage = `mydocker — 自作コンテナランタイム（学習用）

使い方:
  mydocker run [オプション] <イメージ> [コマンド...]   コンテナを起動する
  mydocker ps                                          実行中のコンテナを一覧する
  mydocker exec <ID> <コマンド...>                     実行中のコンテナでコマンドを実行する
  mydocker rm <ID>                                     終了したコンテナの痕跡を片付ける
  mydocker pull <イメージ>                             イメージを取得だけする
  mydocker info                                        この環境で何が使えるかを表示する

run のオプション:
  --memory <量>     メモリ上限（例: 10m, 512k, 1g）
  --cpus <数>       CPU の上限（例: 0.5 で 0.5 コア分）
  --pids <数>       プロセス数の上限
  --hostname <名>   コンテナのホスト名（既定: mydocker）
  --rootfs <パス>   イメージの代わりにローカルのディレクトリを根として使う
  --layer <パス>    重ねるレイヤを直接指定する（下から順に複数回指定できる）
  --quiet           起動時のメッセージを出さない

例:
  mydocker run alpine /bin/sh
  mydocker run --memory 10m alpine sh -c 'echo hello'
  mydocker run --rootfs /var/lib/mydocker/layers/xxxx /bin/sh
`

// Run はコマンドラインを処理する。
func Run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return &ExitError{Code: 2}
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:])
	case "init":
		// ★内部用。利用者が直接叩くものではないので --help には出さない。
		//   親プロセスが /proc/self/exe を使って自分自身を再実行するための入口である。
		return cmdInit(args[1:])
	case "ps":
		return cmdPS(args[1:])
	case "exec":
		return cmdExec(args[1:])
	case "rm":
		return cmdRM(args[1:])
	case "pull":
		return cmdPull(args[1:])
	case "unpack":
		// 内部用（検査用）。tar の展開だけを単体で走らせる。
		return cmdUnpack(args[1:])
	case "info":
		return cmdInfo(args[1:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprintf(os.Stderr, "知らないコマンドです: %s\n\n", args[0])
		fmt.Fprint(os.Stderr, usage)
		return &ExitError{Code: 2}
	}
}

// ---------------------------------------------------------------------------
// run
// ---------------------------------------------------------------------------

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		memory   = fs.String("memory", "", "メモリ上限（例: 10m）")
		cpus     = fs.Float64("cpus", 0, "CPU 上限（例: 0.5）")
		pids     = fs.Int64("pids", 0, "プロセス数の上限")
		hostname = fs.String("hostname", "mydocker", "コンテナのホスト名")
		rootfs   = fs.String("rootfs", "", "ローカルのディレクトリを根として使う")
		layers   stringList
		quiet    = fs.Bool("quiet", false, "起動メッセージを出さない")
	)
	fs.Var(&layers, "layer", "重ねるレイヤ（下から順に複数回指定できる）")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return &ExitError{Code: 2}
	}
	rest := fs.Args()

	mem, err := cgroup.ParseMemory(*memory)
	if err != nil {
		return err
	}
	limits := cgroup.Limits{MemoryBytes: mem, CPUs: *cpus, PidsMax: *pids}

	cfg := container.Config{
		Hostname: *hostname,
		Limits:   limits,
		Quiet:    *quiet,
	}

	switch {
	case *rootfs != "":
		// ローカルの rootfs を最下層として使う。
		// ★元のディレクトリは lowerdir（読み取り専用）として扱うので、
		//   コンテナの中で何をしても元のディレクトリは汚れない。
		abs, err := filepath.Abs(*rootfs)
		if err != nil {
			return err
		}
		cfg.Image = "(local)" + abs
		cfg.Lowers = []string{abs}
		cfg.Argv = rest
	case len(layers) > 0:
		for i, l := range layers {
			abs, err := filepath.Abs(l)
			if err != nil {
				return err
			}
			layers[i] = abs
		}
		cfg.Image = fmt.Sprintf("(layers x%d)", len(layers))
		cfg.Lowers = layers
		cfg.Argv = rest
	default:
		if len(rest) == 0 {
			fmt.Fprint(os.Stderr, usage)
			return errors.New("イメージ名を指定してください")
		}
		store := &image.Store{Root: container.Root, Log: progressWriter(*quiet)}
		img, err := store.Pull(rest[0])
		if err != nil {
			return err
		}
		cfg.Image = img.Ref.String()
		cfg.Lowers = img.LayerDirs
		cfg.Env = img.Env
		cfg.Workdir = img.WorkingDir
		cfg.Argv = append(append([]string{}, img.Entrypoint...), rest[1:]...)
		if len(rest) == 1 {
			// コマンド未指定ならイメージの既定コマンドを使う
			cfg.Argv = append(append([]string{}, img.Entrypoint...), img.Cmd...)
		}
	}

	if len(cfg.Argv) == 0 {
		return errors.New("実行するコマンドを指定してください")
	}
	if len(cfg.Env) == 0 {
		cfg.Env = []string{
			"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			"HOME=/root",
			"TERM=" + os.Getenv("TERM"),
		}
	}
	if err := container.LayerDirsExist(cfg.Lowers); err != nil {
		return err
	}

	code, err := container.Run(cfg)
	if err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}

func progressWriter(quiet bool) io.Writer {
	if quiet {
		return nil
	}
	return os.Stderr
}

// ---------------------------------------------------------------------------
// init（内部用）
// ---------------------------------------------------------------------------

func cmdInit(args []string) error {
	if len(args) < 4 {
		return errors.New("init は mydocker が内部で使うサブコマンドです。直接実行しないでください")
	}
	return container.Init(container.InitArgs{
		Rootfs:   args[0],
		Hostname: args[1],
		Workdir:  args[2],
		Argv:     args[3:],
		Env:      os.Environ(),
	})
}

// ---------------------------------------------------------------------------
// ps
// ---------------------------------------------------------------------------

func cmdPS(args []string) error {
	list, err := container.List()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "CONTAINER ID\tIMAGE\tCOMMAND\tPID\tSTATUS\tCREATED")
	for _, s := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n",
			s.ID, s.Image, strings.Join(s.Command, " "), s.PID, "Up",
			time.Since(s.CreatedAt).Truncate(time.Second))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	// ★件数を実数で出す。「0件でも緑」を防ぐため、テストはこの行を数える。
	fmt.Printf("合計 %d 件\n", len(list))
	return nil
}

// ---------------------------------------------------------------------------
// exec
// ---------------------------------------------------------------------------

func cmdExec(args []string) error {
	if len(args) < 2 {
		return errors.New("使い方: mydocker exec <ID> <コマンド...>")
	}
	st, err := container.Load(args[0])
	if err != nil {
		return err
	}
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/root",
		"TERM=" + os.Getenv("TERM"),
	}
	code, err := container.Exec(st, args[1:], env)
	if err != nil {
		return err
	}
	if code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}

// ---------------------------------------------------------------------------
// rm
// ---------------------------------------------------------------------------

func cmdRM(args []string) error {
	if len(args) < 1 {
		return errors.New("使い方: mydocker rm <ID>")
	}
	for _, id := range args {
		st, err := container.Load(id)
		if err != nil {
			return err
		}
		if err := container.Cleanup(st); err != nil {
			return err
		}
		fmt.Println(st.ID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// pull
// ---------------------------------------------------------------------------

func cmdPull(args []string) error {
	if len(args) < 1 {
		return errors.New("使い方: mydocker pull <イメージ>")
	}
	store := &image.Store{Root: container.Root, Log: os.Stdout}
	img, err := store.Pull(args[0])
	if err != nil {
		return err
	}
	fmt.Printf("%s: レイヤ %d 枚\n", img.Ref, len(img.LayerDirs))
	for i, d := range img.LayerDirs {
		fmt.Printf("  %d: %s\n", i+1, d)
	}
	return nil
}

// ---------------------------------------------------------------------------
// unpack（内部用・検査用）
// ---------------------------------------------------------------------------

func cmdUnpack(args []string) error {
	if len(args) != 2 {
		return errors.New("使い方: mydocker unpack <tar または tar.gz> <展開先>（内部用）")
	}
	f, err := os.Open(args[0])
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := image.ExtractLayer(f, args[1])
	if err != nil {
		return err
	}
	fmt.Printf("%d 件のエントリを展開しました\n", n)
	return nil
}

// ---------------------------------------------------------------------------
// info
// ---------------------------------------------------------------------------

func cmdInfo([]string) error {
	fmt.Printf("状態の置き場: %s\n", container.Root)
	fmt.Printf("資源制限:     %s\n", cgroup.Describe())
	fmt.Printf("overlayfs:    %s\n", yesNo(overlay.Supported()))
	if n, err := overlay.CountMounts("overlay"); err == nil {
		// ★マウントのリークは静かに進行する。件数を実数で出しておけば、
		//   コンテナを何度も起動したあとにこの数が増え続けていないかを目視できる。
		fmt.Printf("overlay 数:   %d 件\n", n)
	}
	list, _ := container.List()
	fmt.Printf("実行中:       %d 件\n", len(list))
	return nil
}

func yesNo(b bool) string {
	if b {
		return "利用可能"
	}
	return "利用不可"
}
