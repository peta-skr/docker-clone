// mydocker — 自作コンテナランタイム（学習用）の入り口。
//
// ここは薄く保つ。引数を internal/cli に渡し、終了コードを整えるだけ。
//
// ⚠ このプログラムは自分自身を再実行する（/proc/self/exe を "init" という顔で起動する）。
// 詳しくは internal/container/run.go の Run を読むこと。
package main

import (
	"errors"
	"fmt"
	"os"

	"mydocker/internal/cli"
)

func main() {
	err := cli.Run(os.Args[1:])
	if err == nil {
		return
	}

	// 「メッセージは出し終わっているので、この終了コードで終われ」という合図。
	// コンテナの中のコマンドの終了コードを、そのまま利用者に返すために使う。
	var ee *cli.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.Code)
	}

	fmt.Fprintf(os.Stderr, "mydocker: %v\n", err)
	os.Exit(1)
}
