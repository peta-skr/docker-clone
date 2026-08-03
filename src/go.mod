// mydocker — 自作コンテナランタイム（学習用）
//
// ★依存ライブラリは1つも使わない。
//   containerd / runc / libcontainer / moby のいずれにも依存しない。
//   使うのは Go の標準ライブラリ（syscall / os/exec / net/http / archive/tar / compress/gzip）だけ。
//   require ブロックが空のままであることが、この教材の前提そのものである。
module mydocker

go 1.24
