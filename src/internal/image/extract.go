package image

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// whiteoutPrefix は「このファイルは上の層で削除された」ことを表す印。
// 例: 下層の /etc/hosts を消したいとき、上層の tar には .wh.hosts というエントリが入る。
const whiteoutPrefix = ".wh."

// whiteoutOpaque は「このディレクトリの下層の中身をまるごと隠す」印。
const whiteoutOpaque = ".wh..wh..opq"

// ErrPathEscape は tar のエントリが展開先の外を指していたことを表す。
// いわゆる Zip Slip。イメージの提供者は自分ではないので、必ず弾く。
var ErrPathEscape = errors.New("tar のエントリが展開先の外を指しています")

// ExtractLayer は tar（gzip 圧縮されていてもいなくてもよい）を dest に展開する。
// 展開したエントリ数を返す。
//
// このレイヤは後で OverlayFS の lowerdir として使うので、
// ホワイトアウトは「消す」のではなく **overlayfs が理解する形に翻訳する**:
//
//	.wh.<名前>     → dest/<名前> にキャラクタデバイス 0:0 を作る（overlay の削除印）
//	.wh..wh..opq   → そのディレクトリに trusted.overlay.opaque="y" を付ける
//
// こうしておくと、重ね合わせたときにカーネル側が下層を隠してくれる。
func ExtractLayer(r io.Reader, dest string) (int, error) {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return 0, err
	}
	realDest, err := filepath.EvalSymlinks(dest)
	if err != nil {
		return 0, err
	}

	tr, closeFn, err := newTarReader(r)
	if err != nil {
		return 0, err
	}
	defer closeFn()

	// ディレクトリのパーミッションと時刻は、中身を書き終えてから当て直す。
	// 先に 0555 のディレクトリを作ってしまうと、その中にファイルを置けなくなるため。
	type dirMeta struct {
		path string
		mode os.FileMode
	}
	var dirs []dirMeta

	count := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return count, fmt.Errorf("tar の読み取りに失敗しました: %w", err)
		}
		// PAX のグローバルヘッダなどは実体を持たない
		if hdr.Typeflag == tar.TypeXGlobalHeader || hdr.Typeflag == tar.TypeXHeader {
			continue
		}

		target, err := safeJoin(realDest, hdr.Name)
		if err != nil {
			return count, fmt.Errorf("%w: %q", err, hdr.Name)
		}

		base := filepath.Base(target)
		parent := filepath.Dir(target)

		// --- ホワイトアウトの処理（普通のファイルとして展開してはいけない）---
		if base == whiteoutOpaque {
			if err := os.MkdirAll(parent, 0o755); err != nil {
				return count, err
			}
			// trusted.* の xattr は CAP_SYS_ADMIN が要る。付けられない環境では
			// 「下層が透けて見える」ことになるので、黙って無視せず警告する。
			if err := syscall.Setxattr(parent, "trusted.overlay.opaque", []byte("y"), 0); err != nil {
				fmt.Fprintf(os.Stderr, "警告: opaque 印を付けられませんでした (%s): %v\n", parent, err)
			}
			count++
			continue
		}
		if strings.HasPrefix(base, whiteoutPrefix) {
			victim := filepath.Join(parent, strings.TrimPrefix(base, whiteoutPrefix))
			if err := os.MkdirAll(parent, 0o755); err != nil {
				return count, err
			}
			_ = os.RemoveAll(victim) // 同じレイヤ内で先に実体が来ていた場合に備える
			// overlayfs の「削除された」印はキャラクタデバイス 0:0 である。
			if err := syscall.Mknod(victim, syscall.S_IFCHR|0o000, 0); err != nil {
				return count, fmt.Errorf("ホワイトアウトを作れませんでした (%s): %w", victim, err)
			}
			count++
			continue
		}

		if err := os.MkdirAll(parent, 0o755); err != nil {
			return count, err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return count, err
			}
			dirs = append(dirs, dirMeta{target, hdr.FileInfo().Mode().Perm()})

		case tar.TypeReg:
			if err := writeFile(target, tr, hdr.FileInfo().Mode().Perm()); err != nil {
				return count, err
			}

		case tar.TypeSymlink:
			// シンボリックリンクの中身は検証しない（リンク先が外を指していても、
			// 実際にそこへ書き込むときに safeJoin が弾く）。
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return count, err
			}

		case tar.TypeLink:
			source, err := safeJoin(realDest, hdr.Linkname)
			if err != nil {
				return count, fmt.Errorf("%w: ハードリンク先 %q", err, hdr.Linkname)
			}
			_ = os.Remove(target)
			if err := os.Link(source, target); err != nil {
				return count, err
			}

		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			mode := uint32(hdr.FileInfo().Mode().Perm())
			switch hdr.Typeflag {
			case tar.TypeChar:
				mode |= syscall.S_IFCHR
			case tar.TypeBlock:
				mode |= syscall.S_IFBLK
			case tar.TypeFifo:
				mode |= syscall.S_IFIFO
			}
			_ = os.Remove(target)
			dev := int(mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor)))
			if err := syscall.Mknod(target, mode, dev); err != nil {
				// 権限が無くてデバイスノードを作れないことはある。致命ではない。
				fmt.Fprintf(os.Stderr, "警告: デバイスノードを作れませんでした (%s): %v\n", target, err)
			}

		default:
			// 未知の型は無視する（GNU の長名ヘッダなどは archive/tar が吸収済み）
			continue
		}

		if hdr.Typeflag != tar.TypeSymlink {
			_ = os.Chown(target, hdr.Uid, hdr.Gid)
		} else {
			_ = os.Lchown(target, hdr.Uid, hdr.Gid)
		}
		count++
	}

	// ディレクトリのパーミッションを後から当てる（深い順に）
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Chmod(dirs[i].path, dirs[i].mode)
	}
	return count, nil
}

// newTarReader は gzip 圧縮されている場合だけ展開してから tar として読む。
// レイヤは通常 tar.gz だが、非圧縮の tar が来ることもあるので先頭2バイトで見分ける。
func newTarReader(r io.Reader) (*tar.Reader, func(), error) {
	br := bufio.NewReader(r)
	magic, err := br.Peek(2)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, func() {}, err
	}
	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, func() {}, fmt.Errorf("gzip として読めませんでした: %w", err)
		}
		return tar.NewReader(zr), func() { zr.Close() }, nil
	}
	return tar.NewReader(br), func() {}, nil
}

// safeJoin は tar のエントリ名を dest 配下の絶対パスに変換する。
// ⚠ ここがイメージ展開のセキュリティの要である。3段で守る:
//
//  1. 絶対パス（"/etc/passwd"）を拒否する
//  2. ".." を含むパス（"../../etc/passwd"）を拒否する
//  3. 途中のディレクトリが dest の外を指すシンボリックリンクになっていないか確認する
//
// 3 が要るのは、レイヤが先に「foo -> /」というリンクを作り、
// 次に "foo/etc/passwd" を書こうとする手口があるため。1 と 2 だけでは通ってしまう。
func safeJoin(realDest, name string) (string, error) {
	if name == "" {
		return "", ErrPathEscape
	}
	// tar の中の区切りは常に "/"
	clean := filepath.Clean("/" + strings.ReplaceAll(name, "\\", "/"))
	if strings.HasPrefix(name, "/") {
		return "", ErrPathEscape
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", ErrPathEscape
		}
	}
	target := filepath.Join(realDest, clean)
	if target != realDest && !strings.HasPrefix(target, realDest+string(os.PathSeparator)) {
		return "", ErrPathEscape
	}

	// 途中の実体がシンボリックリンクで外へ抜けていないかを確かめる。
	// 親ディレクトリが既に存在する場合だけ検査すればよい（無ければこれから作る＝外へは出ない）。
	parent := filepath.Dir(target)
	if realParent, err := filepath.EvalSymlinks(parent); err == nil {
		if realParent != realDest && !strings.HasPrefix(realParent, realDest+string(os.PathSeparator)) {
			return "", ErrPathEscape
		}
	}
	return target, nil
}

func writeFile(target string, r io.Reader, mode os.FileMode) error {
	// 既存を上書きするときは一度消す。シンボリックリンクを開くと
	// リンク先（＝展開先の外かもしれない）に書いてしまうため。
	_ = os.Remove(target)
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// mkdev は major/minor からデバイス番号を組み立てる（Linux の makedev 相当）。
func mkdev(major, minor uint32) uint64 {
	return uint64(minor&0xff) | (uint64(major&0xfff) << 8) |
		((uint64(minor) &^ 0xff) << 12) | ((uint64(major) &^ 0xfff) << 32)
}
