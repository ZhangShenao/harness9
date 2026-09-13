package main

// export_submission：把一轮 run 的产物重排为 SWE-bench 官方提交要求的 artifacts 仓库结构。
//
// 官方目标布局（对齐 swebench submit package 的收集口径）：
//
//	all_preds.jsonl
//	logs/<instance_id>/patch.diff
//	logs/<instance_id>/report.json
//	logs/<instance_id>/test_output.txt.gz
//	trajs/<instance_id>.md
//	EXPORT_MANIFEST.md（导出清单：如实记录哪些实例缺哪些件）
//
// 设计原则：诚实优先。旧 run 往往没有 per-instance 评分产物（report.json /
// test_output），缺失件不伪造、不失败——跳过并写入 EXPORT_MANIFEST.md，
// 让提交者在打 PR 前对缺口一目了然。

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ExportStats 汇总一次导出的产物覆盖情况（供 CLI 回显与测试断言）。
type ExportStats struct {
	Total       int      // predictions.jsonl 中的实例总数
	PatchOK     int      // patch.diff 落盘且非空的实例数
	ReportOK    int      // report.json 就位的实例数
	TestOutOK   int      // test_output.txt.gz 就位的实例数
	TrajOK      int      // trajs/<id>.md 就位的实例数
	Missing     []string // 缺失/异常记录（与 EXPORT_MANIFEST.md 逐条对应）
	CompleteAll bool
}

// evalArtifacts 记录官方 harness 评分产物中定位到的单实例文件路径。
type evalArtifacts struct {
	ReportPath  string
	TestOutPath string
}

// runExport 是 --mode export 的 CLI 入口：校验必填 flag、驱动导出并回显覆盖统计。
func runExport(fromDir, outDir, evalLogsDir string) error {
	if fromDir == "" {
		return fmt.Errorf("export 模式需要 --from 指定 runner run 目录")
	}
	if outDir == "" {
		return fmt.Errorf("export 模式需要 --out 指定导出目录")
	}
	stats, err := exportSubmission(fromDir, outDir, evalLogsDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "导出完成：%d 条实例 → %s\n", stats.Total, outDir)
	fmt.Fprintf(os.Stderr, "  patch.diff 非空 %d ｜ report.json %d ｜ test_output.txt.gz %d ｜ trajs %d\n",
		stats.PatchOK, stats.ReportOK, stats.TestOutOK, stats.TrajOK)
	if len(stats.Missing) > 0 {
		fmt.Fprintf(os.Stderr, "  缺失/异常 %d 条，详见 EXPORT_MANIFEST.md（如实记录，未伪造补齐）\n", len(stats.Missing))
	}
	return nil
}

// readPredictions 读取 predictions.jsonl 全部记录（文件不存在时返回错误）。
func readPredictions(path string) ([]Prediction, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开 predictions.jsonl 失败：%w", err)
	}
	defer f.Close()
	var preds []Prediction
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024) // patch 可能较大，扩大缓冲
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var p Prediction
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			return nil, fmt.Errorf("解析 predictions 行失败：%w", err)
		}
		if p.InstanceID == "" {
			continue
		}
		preds = append(preds, p)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取 predictions.jsonl 失败：%w", err)
	}
	return preds, nil
}

// indexEvalLogs 在 evalLogsDir 下递归定位每个实例的评分产物。
// 官方 harness 的目录层级随版本变化（如 logs/run_evaluation/<run_id>/<instance_id>/...），
// 因此不假设固定深度：任一以 instance_id 命名的目录即视为该实例的产物根，
// report.json 与 test_output.txt[.gz] 在其子树内递归查找（取首个命中）。
// 目录不存在时返回空索引（不报错，由调用方按缺失处理）。
func indexEvalLogs(evalLogsDir string, ids map[string]bool) map[string]*evalArtifacts {
	index := make(map[string]*evalArtifacts)
	entries, err := os.ReadDir(evalLogsDir)
	if err != nil {
		return index
	}
	for _, entry := range entries {
		if !entry.IsDir() || !ids[entry.Name()] {
			continue
		}
		id := entry.Name()
		root := filepath.Join(evalLogsDir, id)
		art := &evalArtifacts{}
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil // 单个子目录不可读不阻断其余产物定位
			}
			name := d.Name()
			switch {
			case art.ReportPath == "" && name == "report.json":
				art.ReportPath = path
			case art.TestOutPath == "" && (name == "test_output.txt" || name == "test_output.txt.gz"):
				art.TestOutPath = path
			}
			return nil
		})
		if art.ReportPath != "" || art.TestOutPath != "" {
			index[id] = art
		}
	}
	return index
}

// exportSubmission 把 fromDir（runner run 目录）的产物导出为 outDir（官方 artifacts 结构）。
// evalLogsDir 为官方 harness 评分产物目录；空串或目录不存在时评分件按缺失记录。
// 返回统计与错误；致命错误仅限 predictions.jsonl 缺失/不可解析，其余一律记 manifest。
func exportSubmission(fromDir, outDir, evalLogsDir string) (*ExportStats, error) {
	preds, err := readPredictions(filepath.Join(fromDir, "predictions.jsonl"))
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Join(outDir, "logs"), 0755); err != nil {
		return nil, fmt.Errorf("创建导出目录失败：%w", err)
	}
	if err := os.MkdirAll(filepath.Join(outDir, "trajs"), 0755); err != nil {
		return nil, fmt.Errorf("创建导出目录失败：%w", err)
	}

	// 1. all_preds.jsonl：逐条重序列化（canonical JSON），是官方评分入口文件。
	if err := writeAllPreds(filepath.Join(outDir, "all_preds.jsonl"), preds); err != nil {
		return nil, err
	}

	// 2. 评分产物索引（目录不存在 → 空索引，全量记缺失）。
	ids := make(map[string]bool, len(preds))
	for _, p := range preds {
		ids[p.InstanceID] = true
	}
	evalIndex := indexEvalLogs(evalLogsDir, ids)

	stats := &ExportStats{Total: len(preds)}
	manifest := &manifestBuilder{OutDir: outDir, FromDir: fromDir, EvalLogsDir: evalLogsDir}

	for _, p := range preds {
		logDir := filepath.Join(outDir, "logs", p.InstanceID)
		if err := os.MkdirAll(logDir, 0755); err != nil {
			return nil, fmt.Errorf("创建 %s 产物目录失败：%w", p.InstanceID, err)
		}

		// patch.diff：model_patch 原样落盘；空 patch 也是该轮的真实结果（如实落盘为空文件，
		// 并在 manifest 标注），不因"没有改动"而省略实例。
		patch := p.ModelPatch
		if patch != "" && !strings.HasSuffix(patch, "\n") {
			patch += "\n" // git apply 要求补丁以换行结尾
		}
		if err := os.WriteFile(filepath.Join(logDir, "patch.diff"), []byte(patch), 0644); err != nil {
			return nil, fmt.Errorf("写入 %s patch.diff 失败：%w", p.InstanceID, err)
		}
		if p.ModelPatch == "" {
			manifest.add(p.InstanceID, "patch.diff", "为空（model_patch 为空，agent 无改动）")
		} else {
			stats.PatchOK++
		}

		// report.json：从评分产物复制；缺失记录。
		art := evalIndex[p.InstanceID]
		if art != nil && art.ReportPath != "" {
			if err := copyFile(filepath.Join(logDir, "report.json"), art.ReportPath); err != nil {
				return nil, fmt.Errorf("复制 %s report.json 失败：%w", p.InstanceID, err)
			}
			stats.ReportOK++
		} else {
			manifest.add(p.InstanceID, "report.json", "缺失（eval-logs 未提供或无该实例评分结果）")
		}

		// test_output.txt.gz：明文则压缩，已压缩则原样复制；缺失记录。
		switch {
		case art != nil && strings.HasSuffix(art.TestOutPath, ".gz"):
			if err := copyFile(filepath.Join(logDir, "test_output.txt.gz"), art.TestOutPath); err != nil {
				return nil, fmt.Errorf("复制 %s test_output.txt.gz 失败：%w", p.InstanceID, err)
			}
			stats.TestOutOK++
		case art != nil && art.TestOutPath != "":
			if err := gzipFile(filepath.Join(logDir, "test_output.txt.gz"), art.TestOutPath); err != nil {
				return nil, fmt.Errorf("压缩 %s test_output.txt 失败：%w", p.InstanceID, err)
			}
			stats.TestOutOK++
		default:
			manifest.add(p.InstanceID, "test_output.txt.gz", "缺失（eval-logs 未提供或无该实例测试输出）")
		}

		// trajs/<id>.md：轨迹取 logs/<RunID>/<id>.log 中最新一代（多次运行的日志互不覆盖，
		// 最新一代即该实例最终交卷轨迹）。
		traj, ok := newestInstanceLog(fromDir, p.InstanceID)
		if ok {
			if err := copyFile(filepath.Join(outDir, "trajs", p.InstanceID+".md"), traj); err != nil {
				return nil, fmt.Errorf("复制 %s 轨迹失败：%w", p.InstanceID, err)
			}
			stats.TrajOK++
		} else {
			manifest.add(p.InstanceID, "trajs/<id>.md", "缺失（run 目录 logs/ 下无该实例轨迹日志）")
		}
	}

	stats.Missing = manifest.entries
	if len(manifest.entries) == 0 {
		stats.CompleteAll = true
	}
	if err := manifest.write(); err != nil {
		return nil, err
	}
	return stats, nil
}

// writeAllPreds 把 predictions 逐条重序列化写入 all_preds.jsonl。
func writeAllPreds(path string, preds []Prediction) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("创建 all_preds.jsonl 失败：%w", err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, p := range preds {
		data, err := json.Marshal(p)
		if err != nil {
			return fmt.Errorf("序列化 prediction 失败：%w", err)
		}
		if _, err := w.Write(append(data, '\n')); err != nil {
			return fmt.Errorf("写入 all_preds.jsonl 失败：%w", err)
		}
	}
	return w.Flush()
}

// newestInstanceLog 在 fromDir/logs/*/ 下查找 <instanceID>.log，返回修改时间最新的一份。
func newestInstanceLog(fromDir, instanceID string) (string, bool) {
	matches, err := filepath.Glob(filepath.Join(fromDir, "logs", "*", instanceID+".log"))
	if err != nil || len(matches) == 0 {
		return "", false
	}
	sort.Slice(matches, func(i, j int) bool {
		ai, aj := fileModTime(matches[i]), fileModTime(matches[j])
		return ai.After(aj)
	})
	return matches[0], true
}

// fileModTime 返回文件修改时间；不可读时返回零值（排序中自然沉底）。
func fileModTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// copyFile 复制源文件到 dst（内容与权限）。
func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("打开源文件失败：%w", err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("创建目标文件失败：%w", err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("复制内容失败：%w", err)
	}
	return nil
}

// gzipFile 把明文 src 压缩为 dst（gzip 格式）。
func gzipFile(dst, src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("读取源文件失败：%w", err)
	}
	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("创建目标文件失败：%w", err)
	}
	zw := gzip.NewWriter(out)
	_, werr := zw.Write(data)
	cerr := zw.Close()
	ferr := out.Close()
	if werr != nil {
		return fmt.Errorf("压缩写入失败：%w", werr)
	}
	if cerr != nil {
		return fmt.Errorf("关闭 gzip writer 失败：%w", cerr)
	}
	if ferr != nil {
		return fmt.Errorf("关闭目标文件失败：%w", ferr)
	}
	return nil
}

// manifestBuilder 聚合导出过程中的缺失/异常记录，最终写入 EXPORT_MANIFEST.md。
type manifestBuilder struct {
	OutDir      string
	FromDir     string
	EvalLogsDir string
	entries     []string // 形如 "<id>|<artifact>|<说明>"
}

// add 记录一条缺失/异常。
func (m *manifestBuilder) add(instanceID, artifact, note string) {
	m.entries = append(m.entries, instanceID+"|"+artifact+"|"+note)
}

// write 渲染并写出 EXPORT_MANIFEST.md（无缺失记录时也写，声明全量完整）。
func (m *manifestBuilder) write() error {
	var b strings.Builder
	b.WriteString("# Export Manifest\n\n")
	fmt.Fprintf(&b, "- Run 目录：%s\n", m.FromDir)
	if m.EvalLogsDir != "" {
		fmt.Fprintf(&b, "- 评分产物目录：%s\n", m.EvalLogsDir)
	} else {
		b.WriteString("- 评分产物目录：未提供（--eval-logs 为空，评分件全部缺失）\n")
	}
	fmt.Fprintf(&b, "- 导出时间：%s\n", time.Now().Format("2006-01-02 15:04:05"))

	if len(m.entries) == 0 {
		b.WriteString("\n全部产物齐备，无缺失件。\n")
	} else {
		fmt.Fprintf(&b, "\n共 %d 条缺失/异常记录：\n\n", len(m.entries))
		b.WriteString("| instance_id | 产物 | 说明 |\n|---|---|---|\n")
		for _, e := range m.entries {
			parts := strings.SplitN(e, "|", 3)
			fmt.Fprintf(&b, "| %s | %s | %s |\n", parts[0], parts[1], parts[2])
		}
		b.WriteString("\n> 以上缺失件未伪造、未补齐；提交前请对照官方 checklist 评估是否补跑评分。\n")
	}

	var buf bytes.Buffer
	buf.WriteString(b.String())
	if err := os.WriteFile(filepath.Join(m.OutDir, "EXPORT_MANIFEST.md"), buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("写入 EXPORT_MANIFEST.md 失败：%w", err)
	}
	return nil
}
