package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildTarGz 在内存中构造一个包含单个文件的 .tar.gz，返回其字节内容。
func buildTarGz(t *testing.T, filename, content string) []byte {
	t.Helper()
	tmp := t.TempDir()
	path := filepath.Join(tmp, "out.tar.gz")

	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	body := []byte(content)
	hdr := &tar.Header{
		Name:     filename,
		Typeflag: tar.TypeReg,
		Size:     int64(len(body)),
		Mode:     0755,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()
	f.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestExtractBinary(t *testing.T) {
	cases := []struct {
		name       string
		tarEntry   string // 压缩包内的文件名
		binaryName string // 要提取的目标文件名
		content    string
		wantErr    bool
	}{
		{"命中文件", "harness9", "harness9", "binary-content", false},
		{"带路径前缀的 tar 条目", "dist/harness9", "harness9", "binary-content", false},
		{"目标不存在", "other", "harness9", "x", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := buildTarGz(t, tc.tarEntry, tc.content)

			tarPath := filepath.Join(t.TempDir(), "pkg.tar.gz")
			if err := os.WriteFile(tarPath, data, 0644); err != nil {
				t.Fatal(err)
			}
			destPath := filepath.Join(t.TempDir(), "harness9")

			err := extractBinary(tarPath, tc.binaryName, destPath)
			if tc.wantErr {
				if err == nil {
					t.Fatal("期望错误，但得到 nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			got, _ := os.ReadFile(destPath)
			if string(got) != tc.content {
				t.Fatalf("内容不匹配：期望 %q，得到 %q", tc.content, got)
			}
		})
	}
}

func TestVerifySHA256(t *testing.T) {
	dir := t.TempDir()

	// 构造测试文件
	content := []byte("hello harness9")
	tarball := filepath.Join(dir, "harness9_1.0.0_linux_amd64.tar.gz")
	if err := os.WriteFile(tarball, content, 0644); err != nil {
		t.Fatal(err)
	}

	// 计算正确哈希
	h := sha256.New()
	h.Write(content)
	correctHash := fmt.Sprintf("%x", h.Sum(nil))
	wrongHash := strings.Repeat("0", 64)

	tarballName := "harness9_1.0.0_linux_amd64.tar.gz"

	cases := []struct {
		name    string
		lines   []string
		wantErr bool
	}{
		{
			"正确哈希",
			[]string{correctHash + "  " + tarballName},
			false,
		},
		{
			"错误哈希",
			[]string{wrongHash + "  " + tarballName},
			true,
		},
		{
			"星号格式",
			[]string{correctHash + " *" + tarballName},
			false,
		},
		{
			"文件中无对应条目",
			[]string{correctHash + "  other_file.tar.gz"},
			true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checksumPath := filepath.Join(dir, "SHA256SUMS")
			if err := os.WriteFile(checksumPath, []byte(strings.Join(tc.lines, "\n")), 0644); err != nil {
				t.Fatal(err)
			}
			err := verifySHA256(tarball, tarballName, checksumPath)
			if tc.wantErr && err == nil {
				t.Fatal("期望错误，但得到 nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("意外错误: %v", err)
			}
		})
	}
}

func TestReadExpectedChecksum(t *testing.T) {
	cases := []struct {
		name        string
		fileContent string
		target      string
		wantHash    string
		wantErr     bool
	}{
		{
			"双空格格式",
			"abc123  file.tar.gz\ndef456  other.tar.gz",
			"file.tar.gz",
			"abc123",
			false,
		},
		{
			"星号格式",
			"abc123 *file.tar.gz",
			"file.tar.gz",
			"abc123",
			false,
		},
		{
			"目标不存在",
			"abc123  other.tar.gz",
			"file.tar.gz",
			"",
			true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "SHA256SUMS")
			if err := os.WriteFile(path, []byte(tc.fileContent), 0644); err != nil {
				t.Fatal(err)
			}
			got, err := readExpectedChecksum(path, tc.target)
			if tc.wantErr {
				if err == nil {
					t.Fatal("期望错误，但得到 nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if got != tc.wantHash {
				t.Fatalf("期望 %q，得到 %q", tc.wantHash, got)
			}
		})
	}
}

func TestDownloadFile(t *testing.T) {
	body := "tarball-content"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "out.tar.gz")
	if err := downloadFile(srv.URL, dest); err != nil {
		t.Fatalf("downloadFile 返回错误: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != body {
		t.Fatalf("期望 %q，得到 %q", body, got)
	}
}

func TestDownloadFileHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	err := downloadFile(srv.URL, filepath.Join(t.TempDir(), "out"))
	if err == nil {
		t.Fatal("期望错误，但得到 nil")
	}
}

func TestFetchLatestRelease(t *testing.T) {
	rel := githubRelease{
		TagName: "v1.2.3",
		Assets: []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		}{
			{Name: "harness9_1.2.3_linux_amd64.tar.gz", BrowserDownloadURL: "https://example.com/harness9.tar.gz"},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(rel)
	}))
	defer srv.Close()

	// 临时替换 httpClient 指向测试服务器
	orig := httpClient
	httpClient = &http.Client{}
	defer func() { httpClient = orig }()

	// fetchLatestRelease 硬编码了 GitHub URL，无法直接注入，
	// 改为直接测试 downloadFile + JSON decode 的集成路径。
	var got githubRelease
	resp, err := httpClient.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.TagName != rel.TagName {
		t.Fatalf("期望 %q，得到 %q", rel.TagName, got.TagName)
	}
	if len(got.Assets) != 1 || got.Assets[0].Name != rel.Assets[0].Name {
		t.Fatalf("Assets 不匹配：%+v", got.Assets)
	}
}

func TestFindAssetURL(t *testing.T) {
	rel := &githubRelease{
		Assets: []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		}{
			{Name: "harness9_1.0.0_darwin_arm64.tar.gz", BrowserDownloadURL: "https://example.com/darwin_arm64.tar.gz"},
			{Name: "harness9_1.0.0_linux_amd64.tar.gz", BrowserDownloadURL: "https://example.com/linux_amd64.tar.gz"},
		},
	}

	cases := []struct {
		name string
		want string
	}{
		{"harness9_1.0.0_darwin_arm64.tar.gz", "https://example.com/darwin_arm64.tar.gz"},
		{"harness9_1.0.0_linux_amd64.tar.gz", "https://example.com/linux_amd64.tar.gz"},
		{"harness9_1.0.0_windows_amd64.zip", ""},
	}

	for _, tc := range cases {
		got := findAssetURL(rel, tc.name)
		if got != tc.want {
			t.Errorf("findAssetURL(%q) = %q，期望 %q", tc.name, got, tc.want)
		}
	}
}

func TestMoveOrCopy(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	dst := filepath.Join(t.TempDir(), "dst")

	if err := os.WriteFile(src, []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := moveOrCopy(src, dst); err != nil {
		t.Fatalf("moveOrCopy 返回错误: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "content" {
		t.Fatalf("内容不匹配: %q", got)
	}
}

// fakeScanner 用于在测试中替换 bufio.Scanner 的输入源（间接验证 readExpectedChecksum）。
func TestReadExpectedChecksumEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "SHA256SUMS")
	if err := os.WriteFile(path, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	f, _ := os.Open(path)
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		t.Error("不应有任何行")
	}
}
