// TUI upgrade 子命令：harness9 自动升级。
// 本文件实现 RunUpgrade，通过 GitHub Releases API 获取最新版本信息，
// 下载对应平台的 tarball，SHA256 校验后原子替换当前二进制文件。
// 通过 goreleaser ldflags 注入的 version 变量比较版本，已是最新时直接返回。
package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
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

// httpClient 带超时的 HTTP 客户端，避免网络不稳定时无限阻塞。
var httpClient = &http.Client{Timeout: 30 * time.Second}

const githubRepo = "ZhangShenao/harness9"

// githubRelease 对应 GitHub Releases API 的响应结构（仅提取所需字段）。
type githubRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// RunUpgrade 检查并升级 harness9 到最新版本。
// currentVersion 由编译时 ldflags 注入（如 "0.0.2"）；本地开发构建为 "dev"。
func RunUpgrade(currentVersion string) error {
	fmt.Println("正在检查最新版本...")

	release, err := fetchLatestRelease()
	if err != nil {
		return err
	}

	latestTag := release.TagName                    // "v0.0.2"
	latestVer := strings.TrimPrefix(latestTag, "v") // "0.0.2"
	currentVer := strings.TrimPrefix(currentVersion, "v")

	if currentVersion == "dev" {
		fmt.Printf("当前版本：dev（开发构建），最新发布版本：%s\n", latestTag)
		fmt.Print("继续将用正式版本覆盖当前二进制，确认升级？[y/N] ")
		var input string
		fmt.Scanln(&input)
		if strings.ToLower(strings.TrimSpace(input)) != "y" {
			fmt.Println("已取消。")
			return nil
		}
	} else if currentVer == latestVer {
		fmt.Printf("当前已是最新版本 %s，无需更新。\n", latestTag)
		return nil
	} else {
		fmt.Printf("发现新版本：%s（当前：%s）\n", latestTag, currentVersion)
	}

	goos := runtime.GOOS
	goarch := runtime.GOARCH

	tarballName := fmt.Sprintf("harness9_%s_%s_%s.tar.gz", latestVer, goos, goarch)
	checksumName := fmt.Sprintf("harness9_%s_SHA256SUMS", latestVer)

	tarballURL := findAssetURL(release, tarballName)
	if tarballURL == "" {
		return fmt.Errorf("未找到适用于 %s/%s 的版本包（%s）", goos, goarch, tarballName)
	}
	checksumURL := findAssetURL(release, checksumName)

	// 下载到临时目录
	tmpDir, err := os.MkdirTemp("", "harness9-upgrade-*")
	if err != nil {
		return fmt.Errorf("创建临时目录失败：%w", err)
	}
	defer os.RemoveAll(tmpDir)

	fmt.Printf("正在下载 %s...\n", tarballName)
	tarballPath := filepath.Join(tmpDir, tarballName)
	if err := downloadFile(tarballURL, tarballPath); err != nil {
		return fmt.Errorf("下载失败：%w", err)
	}

	// SHA256 校验（若 checksumURL 不为空）
	if checksumURL != "" {
		checksumPath := filepath.Join(tmpDir, checksumName)
		if err := downloadFile(checksumURL, checksumPath); err != nil {
			fmt.Fprintf(os.Stderr, "警告：无法下载校验文件，跳过 SHA256 校验: %v\n", err)
		} else {
			fmt.Println("正在校验 SHA256...")
			if err := verifySHA256(tarballPath, tarballName, checksumPath); err != nil {
				return fmt.Errorf("SHA256 校验失败：%w", err)
			}
		}
	}

	// 解压出 harness9 二进制
	binaryPath := filepath.Join(tmpDir, "harness9")
	fmt.Println("正在解压...")
	if err := extractBinary(tarballPath, "harness9", binaryPath); err != nil {
		return err
	}
	if err := os.Chmod(binaryPath, 0755); err != nil {
		return fmt.Errorf("设置执行权限失败：%w", err)
	}

	// 原子替换当前可执行文件
	execPath, err := resolveExecutablePath()
	if err != nil {
		return err
	}

	// 先将新二进制移动到目标目录的临时文件，再重命名（确保跨文件系统时复制）
	destTmp := execPath + ".new"
	if err := moveOrCopy(binaryPath, destTmp); err != nil {
		return fmt.Errorf("写入目标目录失败（可能需要 sudo）: %w", err)
	}
	if err := os.Rename(destTmp, execPath); err != nil {
		os.Remove(destTmp)
		return fmt.Errorf("替换可执行文件失败（可能需要 sudo）: %w", err)
	}

	fmt.Printf("升级成功：harness9 %s\n", latestTag)
	return nil
}

func fetchLatestRelease() (*githubRelease, error) {
	url := "https://api.github.com/repos/" + githubRepo + "/releases/latest"
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("请求 GitHub API 失败：%w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API 返回 %s", resp.Status)
	}

	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("解析版本信息失败：%w", err)
	}
	return &rel, nil
}

func findAssetURL(rel *githubRelease, name string) string {
	for _, a := range rel.Assets {
		if a.Name == name {
			return a.BrowserDownloadURL
		}
	}
	return ""
}

func downloadFile(url, dest string) error {
	resp, err := httpClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 先检查 HTTP 状态码，避免创建空文件后再报错导致磁盘残留。
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}

	if _, err = io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// verifySHA256 从 SHA256SUMS 文件中找到 tarballName 对应的期望摘要，与实际文件比对。
func verifySHA256(tarballPath, tarballName, checksumPath string) error {
	expected, err := readExpectedChecksum(checksumPath, tarballName)
	if err != nil {
		return err
	}

	f, err := os.Open(tarballPath)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	actual := fmt.Sprintf("%x", h.Sum(nil))

	if actual != expected {
		return fmt.Errorf("期望 %s，实际 %s", expected, actual)
	}
	return nil
}

func readExpectedChecksum(checksumPath, tarballName string) (string, error) {
	f, err := os.Open(checksumPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		// 格式：<hash>  <filename> 或 <hash> *<filename>
		parts := strings.Fields(line)
		if len(parts) == 2 {
			name := strings.TrimPrefix(parts[1], "*")
			if name == tarballName {
				return parts[0], nil
			}
		}
	}
	return "", fmt.Errorf("在 SHA256SUMS 中未找到 %s 的校验值", tarballName)
}

// extractBinary 从 .tar.gz 中提取名为 binaryName 的文件到 destPath。
func extractBinary(tarballPath, binaryName, destPath string) error {
	f, err := os.Open(tarballPath)
	if err != nil {
		return fmt.Errorf("打开压缩包失败：%w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("解压 gzip 失败：%w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("读取 tar 失败：%w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if filepath.Base(hdr.Name) != binaryName {
			continue
		}

		out, err := os.Create(destPath)
		if err != nil {
			return fmt.Errorf("创建输出文件失败：%w", err)
		}
		_, copyErr := io.Copy(out, tr)
		out.Close()
		if copyErr != nil {
			return fmt.Errorf("写入二进制文件失败：%w", copyErr)
		}
		return nil
	}
	return fmt.Errorf("在压缩包中未找到 %s", binaryName)
}

func resolveExecutablePath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("获取当前可执行文件路径失败：%w", err)
	}
	p, err = filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("解析符号链接失败：%w", err)
	}
	return p, nil
}

// moveOrCopy 尝试 os.Rename；若跨设备则回退到 copy + 显式 close（确保写入完整）。
func moveOrCopy(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// 跨设备场景：复制内容
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
