package main

import (
	"bufio"
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"time"
)

type processSample struct {
	pid     int
	cpu     float64
	mem     float64
	state   string
	command string
}

type runner struct {
	pid       int
	name      string
	pos       float64
	velocity  float64
	cpu       float64
	mem       float64
	state     string
	lastSeen  time.Time
	deadAt    time.Time
	status    runnerStatus
	maluca    bool
	obstacleS bool
}

type runnerStatus int

const (
	statusRunning runnerStatus = iota
	statusDead
	statusZombie
	statusPitStop
)

type options struct {
	maxLanes   int
	malucaMode bool
	tick       time.Duration
	useEmoji   bool
}

func main() {
	cfg := options{
		maxLanes:   10,
		malucaMode: hasFlag("--maluca") || hasFlag("-m"),
		tick:       600 * time.Millisecond,
		useEmoji:   !hasFlag("--ascii"),
	}

	rand.Seed(time.Now().UnixNano())

	runners := map[int]*runner{}
	ticker := time.NewTicker(cfg.tick)
	defer ticker.Stop()
	defer showCursor()
	hideCursor()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	frame := 0
	for {
		select {
		case <-stop:
			clearScreen()
			return
		default:
		}

		samples, err := pollProcesses()
		if err != nil {
			fmt.Println("failed to read processes:", err)
			time.Sleep(2 * time.Second)
			continue
		}

		now := time.Now()
		for _, s := range samples {
			r, ok := runners[s.pid]
			if !ok {
				r = &runner{pid: s.pid, pos: float64(rand.Intn(5))}
				runners[s.pid] = r
			}
			r.lastSeen = now
			r.name = s.command
			r.cpu = s.cpu
			r.mem = s.mem
			r.state = s.state
			r.maluca = cfg.malucaMode && isChrome(r.name)
			r.status = deriveStatus(r)
			r.velocity = computeVelocity(r)
			r.obstacleS = false
		}

		for pid, r := range runners {
			if now.Sub(r.lastSeen) > 2*time.Second {
				if r.status != statusDead {
					r.status = statusDead
					r.deadAt = now
				}
				if now.Sub(r.deadAt) > 6*time.Second {
					delete(runners, pid)
				}
			}
		}

		lanes := pickLanes(runners, cfg.maxLanes)
		width := trackWidth()
		obstacles := spawnObstacles(lanes)

		updatePositions(lanes, obstacles, cfg.tick.Seconds())

		render(lanes, obstacles, width, frame, cfg)
		frame++

		<-ticker.C
	}
}

func hasFlag(flag string) bool {
	for _, arg := range os.Args[1:] {
		if arg == flag {
			return true
		}
	}
	return false
}

func pollProcesses() ([]processSample, error) {
	cmd := exec.Command("ps", "-axo", "pid,pcpu,pmem,state,comm")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(&out)
	var samples []processSample
	first := true
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if first {
			first = false
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}

		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		cpu, _ := strconv.ParseFloat(fields[1], 64)
		mem, _ := strconv.ParseFloat(fields[2], 64)
		state := fields[3]
		command := strings.Join(fields[4:], " ")
		samples = append(samples, processSample{
			pid:     pid,
			cpu:     cpu,
			mem:     mem,
			state:   state,
			command: command,
		})
	}

	return samples, scanner.Err()
}

func deriveStatus(r *runner) runnerStatus {
	if strings.Contains(strings.ToLower(r.state), "z") {
		return statusZombie
	}
	if r.state != "" && strings.Contains(r.state, "W") {
		return statusPitStop
	}
	return statusRunning
}

func computeVelocity(r *runner) float64 {
	if r.status == statusDead {
		return 0
	}
	if r.status == statusPitStop {
		return 0.2
	}
	if r.status == statusZombie {
		return 0.5
	}

	base := math.Max(r.cpu/25.0, 0.1)
	if r.cpu > 80 {
		base += 2.5
	} else if r.cpu > 50 {
		base += 1.2
	}

	weightPenalty := r.mem / 20.0
	if r.maluca {
		base *= 0.5
		weightPenalty = -0.2 // tanks can barrel through a bit faster
	}

	return math.Max(0.1, base-weightPenalty)
}

func pickLanes(all map[int]*runner, max int) []*runner {
	var list []*runner
	for _, r := range all {
		list = append(list, r)
	}
	// stable-ish: CPU desc then PID asc
	sort.Slice(list, func(i, j int) bool {
		if list[i].cpu == list[j].cpu {
			return list[i].pid < list[j].pid
		}
		return list[i].cpu > list[j].cpu
	})
	if len(list) > max {
		list = list[:max]
	}
	return list
}

func trackWidth() int {
	columns := 100
	if c := os.Getenv("COLUMNS"); c != "" {
		if v, err := strconv.Atoi(c); err == nil && v > 40 {
			columns = v
		}
	}
	// leave room for scoreboard
	if columns > 140 {
		return 90
	}
	return columns - 40
}

func spawnObstacles(lanes []*runner) map[int]struct{} {
	obstacles := map[int]struct{}{}
	if len(lanes) == 0 {
		return obstacles
	}
	var avgCPU float64
	for _, r := range lanes {
		avgCPU += r.cpu
	}
	avgCPU /= float64(len(lanes))
	if avgCPU < 40 {
		return obstacles
	}
	count := 1
	if avgCPU > 80 {
		count = 3
	} else if avgCPU > 60 {
		count = 2
	}
	for i := 0; i < count; i++ {
		obstacles[5+rand.Intn(trackWidth()-10)] = struct{}{}
	}
	return obstacles
}

func updatePositions(lanes []*runner, obstacles map[int]struct{}, delta float64) {
	width := trackWidth()
	for _, r := range lanes {
		if r.status == statusDead {
			continue
		}
		if r.status == statusPitStop {
			continue
		}

		speed := r.velocity
		if _, blocked := obstacles[int(r.pos)]; blocked {
			r.obstacleS = true
			speed *= 0.4
		} else {
			r.obstacleS = false
		}

		r.pos += speed * delta
		if r.pos >= float64(width-2) {
			r.pos = math.Mod(r.pos, float64(width-2))
		}
	}
}

func render(lanes []*runner, obstacles map[int]struct{}, width int, frame int, cfg options) {
	var b strings.Builder
	b.WriteString("\033[H\033[J")
	finish := width - 2
	header := fmt.Sprintf("HTop Gear — %d corredores vivos | modo corrida maluca: %v", len(lanes), cfg.malucaMode)

	trackLines := make([]string, 0, len(lanes)*4)
	for i, r := range lanes {
		sprite := pickSprite(r, cfg)
		carHeight := len(sprite)
		midLine := carHeight / 2
		laneClr := laneColor(i)
		label := fmt.Sprintf("Lane %02d | %-16s CPU:%5.1f MEM:%5.1f %s", i+1, trimName(r.name), r.cpu, r.mem, coloredStatus(r))
		for h := 0; h < carHeight; h++ {
			line := make([]byte, width)
			for j := range line {
				line[j] = ' '
			}
			offset := frame % 8
			for pos := 4 + offset; pos < width-1; pos += 8 {
				line[pos] = '-'
			}
			line[0] = '|'
			line[finish] = '|'
			if h == midLine {
				for pos := range obstacles {
					if pos >= 0 && pos < width {
						line[pos] = '#'
					}
				}
				if r.obstacleS && int(r.pos) < width {
					line[int(r.pos)] = '!'
				}
			}

			coloredCar := colorize(sprite[h], laneClr)
			putSpriteLine(line, int(r.pos), coloredCar)
			prefix := ""
			if h == 0 {
				prefix = label
			}
			trackLines = append(trackLines, fmt.Sprintf("%-40s %s", prefix, string(line)))
		}
	}

	legend := fmt.Sprintf("Legend: %s / %s / %s / %s / %s / %s", colorize("sprint", colorRed), colorize("run", colorGreen), colorize("idle", colorCyan), colorize("pit", colorYellow), colorize("zombie", colorMagenta), colorize("X_X", colorGray))
	leftLines := []string{
		header,
		strings.Repeat("=", len(header)),
		legend,
	}
	leftLines = append(leftLines, trackLines...)

	score := buildScoreboard(lanes)
	if len(score) > 2 {
		// align first runner line with Lane 1 (skip header/separator plus legend)
		score = append(score[:2], append([]string{""}, score[2:]...)...)
	}
	height := max(len(leftLines), len(score))
	pad := width + 60
	for i := 0; i < height; i++ {
		var left, right string
		if i < len(leftLines) {
			left = leftLines[i]
		}
		if i < len(score) {
			right = score[i]
		}
		if right != "" {
			b.WriteString(fmt.Sprintf("%-*s %s\n", pad, left, right))
		} else {
			b.WriteString(left)
			b.WriteByte('\n')
		}
	}

	fmt.Print(b.String())
}

func pickSprite(r *runner, cfg options) []string {
	switch r.status {
	case statusDead:
		return []string{"X_X"}
	case statusZombie:
		return []string{"zZ>"}
	case statusPitStop:
		return []string{"PIT"}
	}

	if cfg.useEmoji {
		car := "🏎️➡️"
		if r.mem > 12 {
			car = "🚛➡️"
		} else if r.mem > 6 {
			car = "🚙➡️"
		}
		if r.maluca {
			car = "[CHR]" + car
		}
		if r.cpu > 70 {
			car += "💨🔥"
		}
		return []string{car}
	}

	car := []string{
		"___      ___        ::::::::::::::::::::::::       [_ _]    [_ _]   _",
		"  /|  ___$________S_   | \\",
		" / |-/        ____  [++| |+",
		"<<<<<---<|  |>____O)<ooo>|",
		" \\ |-\\___ ________ _[++| |+",
		"  \\|    _$_      _S_   |_/  ",
		"       [___]    [___]",
	}

	if r.mem > 12 {
		car[0] = "[P]" + car[0]
	} else if r.mem > 6 {
		car[0] = "[+]" + car[0]
	}
	if r.maluca {
		car[0] = "[CHR]" + car[0]
	}
	if r.cpu > 70 {
		car[len(car)-1] = car[len(car)-1] + ">>"
	}
	return car
}

func putSprite(line []byte, pos int, sprite string) {
	if pos < 0 {
		pos = 0
	}
	if pos >= len(line) {
		pos = len(line) - 1
	}
	for i := 0; i < len(sprite) && pos+i < len(line); i++ {
		line[pos+i] = sprite[i]
	}
}

func putSpriteLine(line []byte, pos int, sprite string) {
	if pos < 0 {
		pos = 0
	}
	if pos >= len(line) {
		pos = len(line) - 1
	}
	for i := 0; i < len(sprite) && pos+i < len(line); i++ {
		ch := sprite[i]
		if ch == '\\' && pos+i < len(line) {
			line[pos+i] = '\\'
			continue
		}
		line[pos+i] = ch
	}
}

func buildScoreboard(lanes []*runner) []string {
	lines := []string{"Placar (PID | CPU% | MEM% | state | status)"}
	lines = append(lines, strings.Repeat("-", 46))
	for i, r := range lanes {
		badge := laneBadge(i)
		entry := fmt.Sprintf("%s %5d %-16s %5.1f %5.1f %-6s %-10s", badge, r.pid, trimName(r.name), r.cpu, r.mem, r.state, coloredStatus(r))
		lines = append(lines, entry)
	}
	return lines
}

func clearScreen() {
	fmt.Print("\033[H\033[2J")
}

func hideCursor() {
	fmt.Print("\033[?25l")
}

func showCursor() {
	fmt.Print("\033[?25h")
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func isChrome(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "chrome") || strings.Contains(n, "chromium")
}

func trimName(name string) string {
	if len(name) <= 16 {
		return name
	}
	return name[:16]
}

const (
	colorReset         = "\033[0m"
	colorRed           = "\033[31m"
	colorGreen         = "\033[32m"
	colorYellow        = "\033[33m"
	colorBlue          = "\033[34m"
	colorMagenta       = "\033[35m"
	colorCyan          = "\033[36m"
	colorGray          = "\033[90m"
	colorBrightRed     = "\033[91m"
	colorBrightGreen   = "\033[92m"
	colorBrightYellow  = "\033[93m"
	colorBrightBlue    = "\033[94m"
	colorBrightMagenta = "\033[95m"
	colorBrightCyan    = "\033[96m"
)

func colorize(s, color string) string {
	return color + s + colorReset
}

func coloredStatus(r *runner) string {
	switch r.status {
	case statusDead:
		return colorize("X_X", colorGray)
	case statusZombie:
		return colorize("zombie", colorMagenta)
	case statusPitStop:
		return colorize("pit", colorYellow)
	default:
		if r.cpu > 70 {
			return colorize("sprint", colorRed)
		}
		if r.cpu < 1 {
			return colorize("idle", colorCyan)
		}
		return colorize("run", colorGreen)
	}
}

func laneColor(idx int) string {
	palette := []string{
		colorRed, colorGreen, colorYellow, colorBlue, colorMagenta, colorCyan,
		colorBrightRed, colorBrightGreen, colorBrightYellow, colorBrightBlue,
		colorBrightMagenta, colorBrightCyan,
	}
	return palette[idx%len(palette)]
}

func laneBadge(idx int) string {
	return colorize("■", laneColor(idx))
}
