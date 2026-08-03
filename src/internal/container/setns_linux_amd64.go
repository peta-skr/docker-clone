package container

// setns(2) のシステムコール番号。
//
// ⚠ Go の標準ライブラリ syscall パッケージには SYS_SETNS が入っていない。
// golang.org/x/sys/unix には Setns があるが、この教材は標準ライブラリだけで書く決まりなので、
// 番号を自分で持ち、syscall.Syscall で直接呼ぶ。
//
// 番号は CPU アーキテクチャごとに違う。ファイル名の _amd64 で自動的に切り替わる。
// 出典: Linux カーネルの arch/x86/entry/syscalls/syscall_64.tbl
const sysSetns = 308
