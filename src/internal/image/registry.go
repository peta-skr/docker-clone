// Package image は Docker Registry HTTP API v2 を直接叩いてイメージを取得し、展開する。
//
// ⚠ docker pull は呼ばない。containerd も使わない。net/http だけで取りに行く。
//
// イメージ取得は3段構えである。
//
//	① トークンを取る   … Docker Hub は匿名でもトークンが要る
//	② マニフェストを取る … 「どのレイヤで出来ているか」の目録
//	③ レイヤを落とす    … 実体は tar.gz の列。それだけ
//
// 「イメージとは tar.gz の列と JSON である」——身も蓋もないが、本当にこれだけである。
package image

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	defaultRegistry = "registry-1.docker.io"
	dockerAuthURL   = "https://auth.docker.io/token"
	dockerService   = "registry.docker.io"
)

// acceptManifest は Accept ヘッダに並べるメディアタイプ。
//
// ⚠ これを付けないと、レジストリは古い形式（schema 1）を返してくる。
// schema 1 には layers 配列が無いので「取れた JSON に layers が無い」と悩むことになる。
var acceptManifest = strings.Join([]string{
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.oci.image.index.v1+json",
}, ",")

// Ref は "alpine" や "ghcr.io/foo/bar:1.2" のようなイメージ参照を分解したもの。
type Ref struct {
	Registry   string // 例: registry-1.docker.io
	Repository string // 例: library/alpine
	Tag        string // 例: latest
}

func (r Ref) String() string { return r.Registry + "/" + r.Repository + ":" + r.Tag }

// cacheKey はキャッシュのファイル名に使える形にした参照。
func (r Ref) cacheKey() string {
	s := r.Registry + "_" + r.Repository + "_" + r.Tag
	return strings.NewReplacer("/", "_", ":", "_").Replace(s)
}

// ParseRef は利用者が打った文字列を Ref に分解する。
//
//	alpine              → registry-1.docker.io / library/alpine : latest
//	alpine:3.20         → registry-1.docker.io / library/alpine : 3.20
//	library/alpine      → registry-1.docker.io / library/alpine : latest
//	ghcr.io/foo/bar:1.2 → ghcr.io             / foo/bar        : 1.2
func ParseRef(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Ref{}, fmt.Errorf("イメージ名が空です")
	}
	ref := Ref{Registry: defaultRegistry, Tag: "latest"}

	name := s
	// 先頭の要素にドットかコロンがあれば、それはレジストリのホスト名である。
	// （"alpine" と "ghcr.io/x" を見分けるための、レジストリ界隈の慣習）
	if i := strings.Index(name, "/"); i >= 0 {
		head := name[:i]
		if strings.ContainsAny(head, ".:") || head == "localhost" {
			ref.Registry = head
			name = name[i+1:]
		}
	}
	// タグの切り出し。ポート番号入りホスト名と混同しないよう、最後の "/" より後ろだけを見る。
	if i := strings.LastIndex(name, ":"); i >= 0 && !strings.Contains(name[i:], "/") {
		ref.Tag = name[i+1:]
		name = name[:i]
	}
	if name == "" {
		return Ref{}, fmt.Errorf("イメージ名が空です: %q", s)
	}
	// Docker Hub の公式イメージは library/ 名前空間に居る
	if ref.Registry == defaultRegistry && !strings.Contains(name, "/") {
		name = "library/" + name
	}
	ref.Repository = name
	return ref, nil
}

// descriptor はマニフェスト中の1項目（レイヤや設定への参照）。
type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Platform  *struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	} `json:"platform,omitempty"`
}

// manifest は「マニフェスト」と「マニフェストリスト（インデックス）」の両方を受けられる形。
// manifests が非 nil ならリスト、layers が非 nil なら本体である。
type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Manifests     []descriptor `json:"manifests"` // インデックスのとき
	Config        descriptor   `json:"config"`    // 本体のとき
	Layers        []descriptor `json:"layers"`    // 本体のとき
}

// imageConfig はイメージの設定 JSON のうち、コンテナ起動に要る部分だけ。
type imageConfig struct {
	Config struct {
		Env        []string `json:"Env"`
		Cmd        []string `json:"Cmd"`
		Entrypoint []string `json:"Entrypoint"`
		WorkingDir string   `json:"WorkingDir"`
	} `json:"config"`
}

// Image は取得・展開が済んだイメージ。
type Image struct {
	Ref        Ref
	LayerDirs  []string // 展開済みレイヤのディレクトリ。★下から上の順（先頭が最下層）
	Env        []string
	Cmd        []string
	Entrypoint []string
	WorkingDir string
}

// Store はイメージのローカル置き場。
//
// ⚠ Docker Hub には匿名アクセスの取得回数制限がある。開発中に何度も引くと止まる。
// レイヤは digest をディレクトリ名にして保存する。digest は中身のハッシュなので、
// 「同じ名前 ＝ 同じ中身」が保証される。これで自然にキャッシュになる。
type Store struct {
	Root string // 例: /var/lib/mydocker
	// Log は進捗の出力先。nil なら何も出さない。
	Log io.Writer
}

func (s *Store) layerDir(digest string) string {
	return filepath.Join(s.Root, "layers", safeDigest(digest))
}
func (s *Store) manifestPath(ref Ref) string {
	return filepath.Join(s.Root, "manifests", ref.cacheKey()+".json")
}
func (s *Store) configPath(digest string) string {
	return filepath.Join(s.Root, "configs", safeDigest(digest)+".json")
}

func (s *Store) logf(format string, args ...any) {
	if s.Log != nil {
		fmt.Fprintf(s.Log, format+"\n", args...)
	}
}

// Pull はイメージを取得して展開し、使える状態にして返す。
// すべてキャッシュに揃っていればネットワークには一切アクセスしない。
func (s *Store) Pull(refStr string) (*Image, error) {
	ref, err := ParseRef(refStr)
	if err != nil {
		return nil, err
	}

	// --- キャッシュだけで済むなら、そもそも通信しない ---
	if img, ok := s.fromCache(ref); ok {
		s.logf("キャッシュ: %s（ネットワークにはアクセスしません）", ref)
		return img, nil
	}

	client := &http.Client{Timeout: 120 * time.Second}

	// ① トークン
	token, err := s.fetchToken(client, ref)
	if err != nil {
		return nil, err
	}

	// ② マニフェスト
	man, err := s.fetchManifest(client, ref, token, ref.Tag)
	if err != nil {
		return nil, err
	}

	// ⚠ 返ってきたのがマニフェストリスト（インデックス）のことがある。
	//    複数 CPU アーキテクチャの入り口なので、amd64/linux の項目を選んで digest で取り直す。
	if len(man.Layers) == 0 && len(man.Manifests) > 0 {
		target, err := pickPlatform(man.Manifests)
		if err != nil {
			return nil, err
		}
		s.logf("マニフェストリストでした。%s/%s の %s を取り直します", runtime.GOOS, runtime.GOARCH, shortDigest(target.Digest))
		man, err = s.fetchManifest(client, ref, token, target.Digest)
		if err != nil {
			return nil, err
		}
	}
	if len(man.Layers) == 0 {
		return nil, fmt.Errorf("%s: マニフェストに layers がありません（schema 1 が返っている可能性があります）", ref)
	}

	// 設定 JSON（Env / Cmd を拾う）
	cfg, err := s.fetchConfig(client, ref, token, man.Config)
	if err != nil {
		return nil, err
	}

	// ③ レイヤ
	var dirs []string
	for i, layer := range man.Layers {
		dir := s.layerDir(layer.Digest)
		if isComplete(dir) {
			s.logf("レイヤ %d/%d %s: キャッシュ済み", i+1, len(man.Layers), shortDigest(layer.Digest))
		} else {
			s.logf("レイヤ %d/%d %s: 取得中 (%.1f MiB)", i+1, len(man.Layers), shortDigest(layer.Digest), float64(layer.Size)/(1<<20))
			if err := s.fetchLayer(client, ref, token, layer, dir); err != nil {
				return nil, err
			}
		}
		dirs = append(dirs, dir)
	}

	// マニフェストを保存しておけば、次回は通信そのものが要らなくなる
	if err := saveJSON(s.manifestPath(ref), man); err != nil {
		return nil, err
	}

	return &Image{
		Ref:        ref,
		LayerDirs:  dirs,
		Env:        cfg.Config.Env,
		Cmd:        cfg.Config.Cmd,
		Entrypoint: cfg.Config.Entrypoint,
		WorkingDir: cfg.Config.WorkingDir,
	}, nil
}

// fromCache は保存済みのマニフェストとレイヤだけでイメージを組み立てられるか試す。
func (s *Store) fromCache(ref Ref) (*Image, bool) {
	var man manifest
	if err := loadJSON(s.manifestPath(ref), &man); err != nil {
		return nil, false
	}
	if len(man.Layers) == 0 {
		return nil, false
	}
	var dirs []string
	for _, l := range man.Layers {
		dir := s.layerDir(l.Digest)
		if !isComplete(dir) {
			return nil, false
		}
		dirs = append(dirs, dir)
	}
	var cfg imageConfig
	if err := loadJSON(s.configPath(man.Config.Digest), &cfg); err != nil {
		return nil, false
	}
	return &Image{
		Ref: ref, LayerDirs: dirs,
		Env: cfg.Config.Env, Cmd: cfg.Config.Cmd,
		Entrypoint: cfg.Config.Entrypoint, WorkingDir: cfg.Config.WorkingDir,
	}, true
}

// ---------------------------------------------------------------------------
// ① トークン
// ---------------------------------------------------------------------------

// fetchToken は pull 用のトークンを取る。
//
// Docker Hub は決まった URL で取れる。それ以外のレジストリは、まず /v2/ を叩いて
// 401 と一緒に返ってくる WWW-Authenticate ヘッダ（＝どこへ取りに行けばよいかの案内）を読む。
func (s *Store) fetchToken(client *http.Client, ref Ref) (string, error) {
	authURL := ""
	if ref.Registry == defaultRegistry {
		authURL = fmt.Sprintf("%s?service=%s&scope=repository:%s:pull", dockerAuthURL, dockerService, ref.Repository)
	} else {
		realm, service, err := discoverAuth(client, ref)
		if err != nil {
			return "", err
		}
		if realm == "" {
			return "", nil // 認証不要のレジストリ
		}
		authURL = fmt.Sprintf("%s?scope=repository:%s:pull", realm, ref.Repository)
		if service != "" {
			authURL += "&service=" + service
		}
	}

	resp, err := client.Get(authURL)
	if err != nil {
		return "", fmt.Errorf("トークンの取得に失敗しました: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("トークンの取得に失敗しました (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("トークンの JSON を読めませんでした: %w", err)
	}
	if tok.Token != "" {
		return tok.Token, nil
	}
	return tok.AccessToken, nil
}

// discoverAuth は /v2/ の 401 応答から認証サーバの場所を読み取る。
func discoverAuth(client *http.Client, ref Ref) (realm, service string, err error) {
	resp, err := client.Get("https://" + ref.Registry + "/v2/")
	if err != nil {
		return "", "", fmt.Errorf("レジストリ %s に接続できませんでした: %w", ref.Registry, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		return "", "", nil
	}
	h := resp.Header.Get("WWW-Authenticate")
	if !strings.HasPrefix(h, "Bearer ") {
		return "", "", nil
	}
	for _, part := range strings.Split(strings.TrimPrefix(h, "Bearer "), ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		v := strings.Trim(kv[1], `"`)
		switch kv[0] {
		case "realm":
			realm = v
		case "service":
			service = v
		}
	}
	return realm, service, nil
}

// ---------------------------------------------------------------------------
// ② マニフェスト
// ---------------------------------------------------------------------------

func (s *Store) fetchManifest(client *http.Client, ref Ref, token, reference string) (*manifest, error) {
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", ref.Registry, ref.Repository, reference)
	body, err := s.get(client, url, token, acceptManifest)
	if err != nil {
		return nil, err
	}
	var man manifest
	if err := json.Unmarshal(body, &man); err != nil {
		return nil, fmt.Errorf("マニフェストの JSON を読めませんでした: %w", err)
	}
	return &man, nil
}

// pickPlatform はマニフェストリストから、この環境で動く1件を選ぶ。
//
// ⚠ 「最初の項目を取る」実装は確実に壊れる。
// リストには architecture が "unknown" の attestation（署名の付随物）が混ざっており、
// それを掴むと layers の無いマニフェストを引いてしまう。
func pickPlatform(list []descriptor) (descriptor, error) {
	for _, d := range list {
		if d.Platform == nil {
			continue
		}
		if d.Platform.OS == runtime.GOOS && d.Platform.Architecture == runtime.GOARCH {
			return d, nil
		}
	}
	var have []string
	for _, d := range list {
		if d.Platform != nil {
			have = append(have, d.Platform.OS+"/"+d.Platform.Architecture)
		}
	}
	return descriptor{}, fmt.Errorf("このイメージに %s/%s 向けの版がありません（あるのは: %s）",
		runtime.GOOS, runtime.GOARCH, strings.Join(have, ", "))
}

func (s *Store) fetchConfig(client *http.Client, ref Ref, token string, d descriptor) (*imageConfig, error) {
	var cfg imageConfig
	path := s.configPath(d.Digest)
	if err := loadJSON(path, &cfg); err == nil {
		return &cfg, nil
	}
	url := fmt.Sprintf("https://%s/v2/%s/blobs/%s", ref.Registry, ref.Repository, d.Digest)
	body, err := s.get(client, url, token, "*/*")
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("イメージ設定の JSON を読めませんでした: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ---------------------------------------------------------------------------
// ③ レイヤ
// ---------------------------------------------------------------------------

// fetchLayer はレイヤ1枚を落として dir に展開する。
//
// 途中で失敗したディレクトリを「取得済み」と誤認しないよう、
// 展開が最後まで終わったときにだけ .complete という印を置く。
func (s *Store) fetchLayer(client *http.Client, ref Ref, token string, d descriptor, dir string) error {
	url := fmt.Sprintf("https://%s/v2/%s/blobs/%s", ref.Registry, ref.Repository, d.Digest)
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("レイヤの取得に失敗しました: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("レイヤの取得に失敗しました (HTTP %d): %s", resp.StatusCode, shortDigest(d.Digest))
	}

	// 落としながら同時にハッシュを計算する。digest は中身のハッシュなので、
	// 一致しなければ「途中で壊れた」か「別物を掴んだ」ことになる。
	hasher := sha256.New()
	body := io.TeeReader(resp.Body, hasher)

	tmp := dir + ".tmp"
	_ = os.RemoveAll(tmp)
	if _, err := ExtractLayer(body, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("レイヤ %s の展開に失敗しました: %w", shortDigest(d.Digest), err)
	}
	// 本体を読み切ってからでないとハッシュが完成しない
	if _, err := io.Copy(io.Discard, body); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	got := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(got, d.Digest) {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("レイヤの digest が一致しません: 期待 %s / 実際 %s", d.Digest, got)
	}

	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return err
	}
	return os.WriteFile(dir+".complete", []byte(d.Digest), 0o644)
}

// ---------------------------------------------------------------------------
// 小道具
// ---------------------------------------------------------------------------

func (s *Store) get(client *http.Client, url, token, accept string) ([]byte, error) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", accept)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s の取得に失敗しました: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("見つかりませんでした (HTTP 404): %s", url)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("取得回数の上限に達しました (HTTP 429)。しばらく待つか、キャッシュ済みのイメージを使ってください")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("取得に失敗しました (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return io.ReadAll(resp.Body)
}

func isComplete(dir string) bool {
	if _, err := os.Stat(dir + ".complete"); err != nil {
		return false
	}
	fi, err := os.Stat(dir)
	return err == nil && fi.IsDir()
}

func safeDigest(d string) string {
	return strings.NewReplacer(":", "_", "/", "_").Replace(d)
}

func shortDigest(d string) string {
	if i := strings.Index(d, ":"); i >= 0 && len(d) > i+13 {
		return d[i+1 : i+13]
	}
	return d
}

func saveJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func loadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
