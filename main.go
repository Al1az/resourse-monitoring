package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

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

	// Для расчета загрузки процессора нам нужно хранить предыдущие значения
	var prevIdle, prevTotal uint64

	fmt.Println("Запуск системного монитора (Linux /proc)... Нажмите Ctrl+C для выхода")
	time.Sleep(500 * time.Millisecond)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Очистка экрана
			fmt.Print("\033[H\033[2J")

			fmt.Println("==================================================")
			fmt.Printf("   МОНИТОР РЕСУРСОВ | %s\n", time.Now().Format("15:04:05"))
			fmt.Println("==================================================")

			// 1. Сбор CPU
			idle, total := getCPUTicks()
			if prevTotal > 0 {
				diffIdle := idle - prevIdle
				diffTotal := total - prevTotal
				if diffTotal > 0 {
					cpuPercent := (float64(diffTotal-diffIdle) / float64(diffTotal)) * 100
					fmt.Printf("CPU:    [%-20s] %.2f%%\n", renderBars(cpuPercent), cpuPercent)
				}
			} else {
				fmt.Println("CPU:    Расчет (пожалуйста, подождите)...")
			}
			prevIdle, prevTotal = idle, total

			// 2. Сбор RAM
			memTotal, memAvailable := getMemoryInfo()
			if memTotal > 0 {
				memUsed := memTotal - memAvailable
				memPercent := (float64(memUsed) / float64(memTotal)) * 100
				fmt.Printf("RAM:    [%-20s] %.2f / %.2f GB (%.2f%%)\n",
					renderBars(memPercent),
					float64(memUsed)/(1024*1024),
					float64(memTotal)/(1024*1024),
					memPercent,
				)
			}

			// 3. Сбор Диска (Используем syscall, который встроен в стандартную библиотеку Go)
			var stat syscall.Statfs_t
			err := syscall.Statfs("/", &stat)
			if err == nil {
				allBytes := stat.Blocks * uint64(stat.Bsize)
				freeBytes := stat.Bfree * uint64(stat.Bsize)
				usedBytes := allBytes - freeBytes
				diskPercent := (float64(usedBytes) / float64(allBytes)) * 100

				fmt.Printf("Disk /: [%-20s] %.2f / %.2f GB (%.2f%%)\n",
					renderBars(diskPercent),
					float64(usedBytes)/(1024*1024*1024),
					float64(allBytes)/(1024*1024*1024),
					diskPercent,
				)
			}

			fmt.Println("==================================================")
		}
	}
}

// getCPUTicks парсит /proc/stat для расчета процессорного времени
func getCPUTicks() (idle, total uint64) {
	file, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	if scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "cpu" {
			return 0, 0
		}

		// Суммируем все тики процессора
		for i := 1; i < len(fields); i++ {
			val, _ := strconv.ParseUint(fields[i], 10, 64)
			total += val
			if i == 4 { // 4-е поле (индекс 4) — это время бездействия (idle)
				idle = val
			}
		}
	}
	return idle, total
}

// getMemoryInfo парсит /proc/meminfo (значения возвращаются в килобайтах)
func getMemoryInfo() (total, available uint64) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		if fields[0] == "MemTotal:" {
			total, _ = strconv.ParseUint(fields[1], 10, 64)
		}
		if fields[0] == "MemAvailable:" {
			available, _ = strconv.ParseUint(fields[1], 10, 64)
		}
	}
	return total, available
}

// Текстовый прогресс-бар
func renderBars(percent float64) string {
	bars := int(percent / 5)
	if bars > 20 {
		bars = 20
	}
	result := ""
	for i := 0; i < 20; i++ {
		if i < bars {
			result += "|"
		} else {
			result += "."
		}
	}
	return result
}
