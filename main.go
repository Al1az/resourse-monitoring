package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Ширина "внутренней" области макета — под неё подгоняются рамка и разделители секций.
const layoutWidth = 60

// USER_HZ — стандартная частота тиков ядра Linux на большинстве систем x86/x86_64.
// Используется для перевода utime/stime процессов из тиков в секунды.
const clockTicksPerSec = 100.0

func main() {
	// Контекст для безопасного завершения горутин
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Перехват Ctrl+C
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigs
		fmt.Println("\n\nЗавершение работы...")
		cancel()
	}()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	// --- Состояние между итерациями ---
	var prevCPU CoreStat
	var prevCores []CoreStat
	prevNet := getNetStat()
	prevProcTicks := make(map[int]uint64)
	collectProcesses(prevProcTicks, 0) // прайминг карты тиков процессов
	prevSampleTime := time.Now()

	fmt.Println("Запуск системного монитора (Linux /proc)... Нажмите Ctrl+C для выхода")
	time.Sleep(500 * time.Millisecond)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			elapsed := now.Sub(prevSampleTime).Seconds()
			prevSampleTime = now

			// Очистка экрана (менее «мигающий» вариант, чем \033[2J)
			fmt.Print("\033[H\033[J")

			printHeaderBox(now)

			// ===== БЛОК: ПРОЦЕССОР =====
			fmt.Println()
			fmt.Println(sectionHeader("ПРОЦЕССОР"))
			totalCPU, cores := readCPUStats()
			if prevCPU.Total > 0 {
				diffIdle := totalCPU.Idle - prevCPU.Idle
				diffTotal := totalCPU.Total - prevCPU.Total
				if diffTotal > 0 {
					cpuPercent := float64(diffTotal-diffIdle) / float64(diffTotal) * 100
					fmt.Printf("  %-6s [%-20s] %6.2f%%\n", "ВСЕГО:", renderBar(cpuPercent, 20), cpuPercent)
				}
			} else {
				fmt.Println("  ВСЕГО:  расчет (пожалуйста, подождите)...")
			}
			for _, line := range renderCoreLines(cores, prevCores) {
				fmt.Println(line)
			}
			prevCPU, prevCores = totalCPU, cores

			// ===== БЛОК: ПАМЯТЬ =====
			fmt.Println()
			fmt.Println(sectionHeader("ПАМЯТЬ"))
			mem := getMemoryInfo()
			if mem.Total > 0 {
				memUsedKb := mem.Total - mem.Available
				memPercent := float64(memUsedKb) / float64(mem.Total) * 100
				fmt.Printf("  RAM: [%-20s] %.2f / %.2f GB (%.2f%%)\n",
					renderBar(memPercent, 20),
					float64(memUsedKb)/(1024*1024),
					float64(mem.Total)/(1024*1024),
					memPercent,
				)
			} else {
				fmt.Println("  RAM: нет данных")
			}

			// ===== БЛОК: ДИСК =====
			fmt.Println()
			fmt.Println(sectionHeader("ДИСК"))
			if disk, err := getDiskInfo("/"); err == nil {
				fmt.Printf("  /:   [%-20s] %.2f / %.2f GB (%.2f%%)\n",
					renderBar(disk.Percent, 20),
					float64(disk.Used)/(1024*1024*1024),
					float64(disk.Total)/(1024*1024*1024),
					disk.Percent,
				)
			} else {
				fmt.Println("  /:   нет данных")
			}

			// ===== БЛОК: СЕТЬ =====
			fmt.Println()
			fmt.Println(sectionHeader("СЕТЬ"))
			net := getNetStat()
			if elapsed > 0 {
				rxRate := float64(net.RxBytes-prevNet.RxBytes) / elapsed
				txRate := float64(net.TxBytes-prevNet.TxBytes) / elapsed
				fmt.Printf("  ↓ %-12s  ↑ %s\n", formatBytesPerSec(rxRate), formatBytesPerSec(txRate))
			} else {
				fmt.Println("  расчет...")
			}
			prevNet = net

			// ===== БЛОК: ВИДЕОКАРТА =====
			fmt.Println()
			fmt.Println(sectionHeader("ВИДЕОКАРТА"))
			gpus, source := getGPUInfo()
			if source == "none" {
				fmt.Println("  не обнаружено (нет nvidia-smi / amdgpu sysfs)")
			} else {
				for _, g := range gpus {
					memPercent := 0.0
					if g.MemTotalMB > 0 {
						memPercent = g.MemUsedMB / g.MemTotalMB * 100
					}
					fmt.Printf("  GPU %d: [%-20s] %5.1f%%  %s\n", g.Index, renderBar(g.UtilPercent, 20), g.UtilPercent, g.Name)
					fmt.Printf("          VRAM: %.0f / %.0f MB (%.1f%%)", g.MemUsedMB, g.MemTotalMB, memPercent)
					if g.TempC > 0 {
						fmt.Printf("   Темп: %.0f°C", g.TempC)
					}
					fmt.Println()
				}
			}

			// ===== БЛОК: ПРОЦЕССЫ =====
			fmt.Println()
			fmt.Println(sectionHeader("ПРОЦЕССЫ"))
			procs := collectProcesses(prevProcTicks, elapsed)
			fmt.Println("  Топ-5 по CPU:")
			printProcTable(topByCPU(procs, 5))
			fmt.Println()
			fmt.Println("  Топ-5 по RAM:")
			printProcTable(topByRSS(procs, 5))

			fmt.Println()
			fmt.Println(strings.Repeat("─", layoutWidth+2))
			fmt.Println("  Ctrl+C — выход")
		}
	}
}

// ====================== CPU ======================

// CoreStat хранит тики простоя и суммарные тики для одного ядра (или для CPU в целом).
type CoreStat struct {
	Name  string
	Idle  uint64
	Total uint64
}

// readCPUStats парсит /proc/stat: первая строка "cpu" — агрегат,
// строки "cpu0", "cpu1"... — отдельные ядра.
func readCPUStats() (total CoreStat, cores []CoreStat) {
	file, err := os.Open("/proc/stat")
	if err != nil {
		return total, cores
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}

		var idle, sum uint64
		for i := 1; i < len(fields); i++ {
			val, _ := strconv.ParseUint(fields[i], 10, 64)
			sum += val
			if i == 4 { // 4-е поле — время бездействия (idle)
				idle = val
			}
		}

		cs := CoreStat{Name: fields[0], Idle: idle, Total: sum}
		if fields[0] == "cpu" {
			total = cs
		} else {
			cores = append(cores, cs)
		}
	}
	return total, cores
}

// renderCoreLines формирует строки для вывода загрузки по ядрам.
// При большом количестве ядер (>8) переходит на компактный режим (по 4 ядра в строке).
func renderCoreLines(cores, prevCores []CoreStat) []string {
	n := len(cores)
	if n == 0 {
		return nil
	}

	type corePct struct {
		name  string
		pct   float64
		ready bool
	}

	pcts := make([]corePct, n)
	for i, c := range cores {
		cp := corePct{name: strings.ToUpper(c.Name)}
		if i < len(prevCores) {
			prev := prevCores[i]
			diffTotal := c.Total - prev.Total
			diffIdle := c.Idle - prev.Idle
			if diffTotal > 0 {
				cp.pct = float64(diffTotal-diffIdle) / float64(diffTotal) * 100
				cp.ready = true
			}
		}
		pcts[i] = cp
	}

	var lines []string
	if n <= 8 {
		barWidth := 20
		for _, cp := range pcts {
			if cp.ready {
				lines = append(lines, fmt.Sprintf("  %-6s [%-*s] %5.1f%%", cp.name, barWidth, renderBar(cp.pct, barWidth), cp.pct))
			} else {
				lines = append(lines, fmt.Sprintf("  %-6s расчет...", cp.name))
			}
		}
		return lines
	}

	// Компактный режим: по 4 ядра в строке, бар короче
	barWidth := 10
	var sb strings.Builder
	count := 0
	for i, cp := range pcts {
		var seg string
		if cp.ready {
			seg = fmt.Sprintf("%-5s[%-*s]%5.1f%%", cp.name, barWidth, renderBar(cp.pct, barWidth), cp.pct)
		} else {
			seg = fmt.Sprintf("%-5s расчет     ", cp.name)
		}
		sb.WriteString(seg)
		count++
		if count == 4 || i == n-1 {
			lines = append(lines, "  "+sb.String())
			sb.Reset()
			count = 0
		} else {
			sb.WriteString("  ")
		}
	}
	return lines
}

// ====================== RAM ======================

type MemInfo struct {
	Total, Available, Free, Buffers, Cached uint64 // в килобайтах
}

// getMemoryInfo парсит /proc/meminfo. Если MemAvailable отсутствует
// (ядра старше 3.14), используется фолбэк: Free + Buffers + Cached.
func getMemoryInfo() MemInfo {
	var m MemInfo
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return m
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		val, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			m.Total = val
		case "MemAvailable:":
			m.Available = val
		case "MemFree:":
			m.Free = val
		case "Buffers:":
			m.Buffers = val
		case "Cached:":
			m.Cached = val
		}
	}

	if m.Available == 0 {
		m.Available = m.Free + m.Buffers + m.Cached
	}
	return m
}

// ====================== Disk ======================

type DiskInfo struct {
	Total, Used, Free uint64
	Percent           float64
}

func getDiskInfo(path string) (DiskInfo, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return DiskInfo{}, err
	}

	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bfree * uint64(stat.Bsize)
	used := total - free

	var percent float64
	if total > 0 {
		percent = float64(used) / float64(total) * 100
	}
	return DiskInfo{Total: total, Used: used, Free: free, Percent: percent}, nil
}

// ====================== Network ======================

type NetStat struct {
	RxBytes, TxBytes uint64
}

// getNetStat суммирует входящий/исходящий трафик по всем интерфейсам,
// кроме loopback (lo), парся /proc/net/dev.
func getNetStat() NetStat {
	var ns NetStat
	file, err := os.Open("/proc/net/dev")
	if err != nil {
		return ns
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		if lineNum <= 2 {
			continue // две строки заголовков
		}
		parts := strings.SplitN(scanner.Text(), ":", 2)
		if len(parts) != 2 {
			continue
		}
		iface := strings.TrimSpace(parts[0])
		if iface == "lo" {
			continue
		}
		fields := strings.Fields(parts[1])
		if len(fields) < 9 {
			continue
		}
		rx, _ := strconv.ParseUint(fields[0], 10, 64) // receive bytes
		tx, _ := strconv.ParseUint(fields[8], 10, 64) // transmit bytes
		ns.RxBytes += rx
		ns.TxBytes += tx
	}
	return ns
}

func formatBytesPerSec(v float64) string {
	switch {
	case v >= 1024*1024:
		return fmt.Sprintf("%.2f MB/s", v/(1024*1024))
	case v >= 1024:
		return fmt.Sprintf("%.2f KB/s", v/1024)
	default:
		return fmt.Sprintf("%.0f B/s", v)
	}
}

// ====================== GPU ======================

type GPUInfo struct {
	Index                 int
	Name                  string
	UtilPercent           float64
	MemUsedMB, MemTotalMB float64
	TempC                 float64
}

// getGPUInfo пытается определить загрузку GPU:
//  1. NVIDIA — через утилиту nvidia-smi (если установлена)
//  2. AMD    — через sysfs (драйвер amdgpu: gpu_busy_percent, mem_info_vram_*)
//
// Если ни один источник не найден, возвращает source == "none".
func getGPUInfo() (gpus []GPUInfo, source string) {
	if nvGpus, ok := getNvidiaGPUInfo(); ok {
		return nvGpus, "nvidia-smi"
	}
	if amdGpus, ok := getAMDGPUInfo(); ok {
		return amdGpus, "amdgpu-sysfs"
	}
	return nil, "none"
}

func getNvidiaGPUInfo() ([]GPUInfo, bool) {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil, false
	}

	out, err := exec.Command(path,
		"--query-gpu=index,name,utilization.gpu,memory.used,memory.total,temperature.gpu",
		"--format=csv,noheader,nounits",
	).Output()
	if err != nil {
		return nil, false
	}

	var gpus []GPUInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 6 {
			continue
		}
		idx, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
		util, _ := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64)
		memUsed, _ := strconv.ParseFloat(strings.TrimSpace(parts[3]), 64)
		memTotal, _ := strconv.ParseFloat(strings.TrimSpace(parts[4]), 64)
		temp, _ := strconv.ParseFloat(strings.TrimSpace(parts[5]), 64)
		gpus = append(gpus, GPUInfo{
			Index: idx, Name: strings.TrimSpace(parts[1]),
			UtilPercent: util, MemUsedMB: memUsed, MemTotalMB: memTotal, TempC: temp,
		})
	}
	return gpus, len(gpus) > 0
}

func getAMDGPUInfo() ([]GPUInfo, bool) {
	busyFiles, _ := filepath.Glob("/sys/class/drm/card[0-9]*/device/gpu_busy_percent")
	if len(busyFiles) == 0 {
		return nil, false
	}

	var gpus []GPUInfo
	for i, busyPath := range busyFiles {
		dir := filepath.Dir(busyPath)

		data, err := os.ReadFile(busyPath)
		if err != nil {
			continue
		}
		util, _ := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)

		var memUsedMB, memTotalMB float64
		if d, err := os.ReadFile(filepath.Join(dir, "mem_info_vram_used")); err == nil {
			v, _ := strconv.ParseUint(strings.TrimSpace(string(d)), 10, 64)
			memUsedMB = float64(v) / (1024 * 1024)
		}
		if d, err := os.ReadFile(filepath.Join(dir, "mem_info_vram_total")); err == nil {
			v, _ := strconv.ParseUint(strings.TrimSpace(string(d)), 10, 64)
			memTotalMB = float64(v) / (1024 * 1024)
		}

		cardName := filepath.Base(filepath.Dir(filepath.Dir(busyPath))) // например "card0"
		gpus = append(gpus, GPUInfo{
			Index: i, Name: "AMD GPU (" + cardName + ")",
			UtilPercent: util, MemUsedMB: memUsedMB, MemTotalMB: memTotalMB,
		})
	}
	return gpus, len(gpus) > 0
}

// ====================== Processes ======================

type ProcInfo struct {
	PID        int
	Name       string
	CPUPercent float64
	RSSKb      uint64
}

// collectProcesses обходит /proc/[pid], считая CPU% (по дельте utime+stime
// от прошлой выборки) и текущий RSS. Карта prevTicks обновляется in-place —
// её нужно передавать той же переменной на следующей итерации.
func collectProcesses(prevTicks map[int]uint64, elapsed float64) []ProcInfo {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}

	var procs []ProcInfo
	currentTicks := make(map[int]uint64)

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}

		statData, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue // процесс мог завершиться между чтением каталога и файла
		}
		content := string(statData)

		openParen := strings.Index(content, "(")
		closeParen := strings.LastIndex(content, ")")
		if openParen == -1 || closeParen == -1 || closeParen < openParen {
			continue
		}
		name := content[openParen+1 : closeParen]

		rest := strings.Fields(content[closeParen+1:])
		if len(rest) < 13 {
			continue
		}
		utime, _ := strconv.ParseUint(rest[11], 10, 64) // поле 14 в /proc/[pid]/stat
		stime, _ := strconv.ParseUint(rest[12], 10, 64) // поле 15
		totalTicks := utime + stime
		currentTicks[pid] = totalTicks

		var cpuPercent float64
		if prev, ok := prevTicks[pid]; ok && elapsed > 0 && totalTicks >= prev {
			delta := totalTicks - prev
			cpuPercent = float64(delta) / clockTicksPerSec / elapsed * 100
		}

		var rssKb uint64
		if statusData, err := os.ReadFile(filepath.Join("/proc", e.Name(), "status")); err == nil {
			for _, line := range strings.Split(string(statusData), "\n") {
				if strings.HasPrefix(line, "VmRSS:") {
					fields := strings.Fields(line)
					if len(fields) >= 2 {
						rssKb, _ = strconv.ParseUint(fields[1], 10, 64)
					}
					break
				}
			}
		}

		procs = append(procs, ProcInfo{PID: pid, Name: name, CPUPercent: cpuPercent, RSSKb: rssKb})
	}

	// Перезаписываем карту предыдущих тиков для следующей итерации
	for k := range prevTicks {
		delete(prevTicks, k)
	}
	for k, v := range currentTicks {
		prevTicks[k] = v
	}

	return procs
}

func topByCPU(procs []ProcInfo, n int) []ProcInfo {
	sorted := append([]ProcInfo(nil), procs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].CPUPercent > sorted[j].CPUPercent })
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	return sorted
}

func topByRSS(procs []ProcInfo, n int) []ProcInfo {
	sorted := append([]ProcInfo(nil), procs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].RSSKb > sorted[j].RSSKb })
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	return sorted
}

func printProcTable(procs []ProcInfo) {
	fmt.Printf("    %-8s %-22s %8s %10s\n", "PID", "ИМЯ", "CPU%", "RSS(MB)")
	for _, p := range procs {
		fmt.Printf("    %-8d %-22s %7.1f%% %9.1f\n", p.PID, truncate(p.Name, 22), p.CPUPercent, float64(p.RSSKb)/1024)
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}

// ====================== Макет: рамка и заголовки секций ======================

// printHeaderBox печатает верхнюю рамку с названием монитора и текущим временем.
// Ширина подгоняется под layoutWidth, выравнивание учитывает кириллицу (рунами, не байтами).
func printHeaderBox(now time.Time) {
	fmt.Println("╔" + strings.Repeat("═", layoutWidth) + "╗")

	left := "  МОНИТОР РЕСУРСОВ"
	right := now.Format("15:04:05") + "  "
	gap := layoutWidth - utf8.RuneCountInString(left) - utf8.RuneCountInString(right)
	if gap < 1 {
		gap = 1
	}
	content := left + strings.Repeat(" ", gap) + right
	fmt.Println("║" + content + "║")

	fmt.Println("╚" + strings.Repeat("═", layoutWidth) + "╝")
}

// sectionHeader формирует заголовок блока вида "── ПРОЦЕССОР ──────...",
// растянутый до layoutWidth с учётом реальной (рунной) длины названия.
func sectionHeader(title string) string {
	prefix := "── " + title + " "
	rl := utf8.RuneCountInString(prefix)
	if rl >= layoutWidth {
		return prefix
	}
	return prefix + strings.Repeat("─", layoutWidth-rl)
}

// ====================== Общий рендер прогресс-бара ======================

func renderBar(percent float64, width int) string {
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	filled := int(percent / 100 * float64(width))
	if filled > width {
		filled = width
	}
	return strings.Repeat("|", filled) + strings.Repeat(".", width-filled)
}
