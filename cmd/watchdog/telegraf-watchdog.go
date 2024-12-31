package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"container/list"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ProcessInfo 结构体用于存储进程信息
type ProcessInfo struct {
	PID       int    `json:"pid"`
	Directory string `json:"directory"`
	StartTime string `json:"start_time"`
	Uptime    string `json:"uptime"`
}

// Config 结构体用于存储配置信息
type Config struct {
	TelegrafExecutable string `json:"telegrafExecutable"`
	ConfigFile         string `json:"configFile"`
	HTTPPort           string `json:"httpPort"`
	Username           string `json:"username"` // 新增用户名字段
	Password           string `json:"password"` // 新增密码字段
	EnableAuth         bool   `json:"enableAuth"` // 新增是否开启HTTP认证的开关
}

var (
	telegrafCmd  *exec.Cmd
	processInfo  sync.Map
	shouldDaemon bool
	shouldRun    bool
	config       Config
	mutex        sync.Mutex
	scriptDir    = "../script"
	logFile      string // 存储从 Telegraf 配置文件中读取的日志文件路径
)

func main() {
	// 加载配置文件
	loadConfig()

	// 初始化标志变量
	shouldDaemon = true
	shouldRun = false

	// 创建 script 目录（如果不存在）
	createScriptDir()

	// 启动 HTTP 服务器
	go startHTTPServer()

	// 持续监控 Telegraf 进程
	for {
		if shouldDaemon && !isTelegrafRunning() {
			log.Println("Telegraf is not running, starting and daemonizing...")
			startTelegraf()
		}
		time.Sleep(5 * time.Second)
	}
}

func loadConfig() {
	data, err := os.ReadFile("../conf/watchdog.json")
	if err != nil {
		log.Fatalf("Failed to read config file: %v", err)
	}
	err = json.Unmarshal(data, &config)
	if err != nil {
		log.Fatalf("Failed to parse config file: %v", err)
	}

	// 检查配置文件是否存在
	if _, err := os.Stat(config.ConfigFile); os.IsNotExist(err) {
		log.Fatalf("Telegraf config file doesades not exist: %s", config.ConfigFile)
	}

	// 从 Telegraf 配置文件中读取日志文件路径
	logFile = getLogFileFromTelegrafConfig(config.ConfigFile)
	if logFile == "" {
		log.Println("Log file path not found in Telegraf configuration.")
	} else {
		log.Printf("Log file path from Telegraf config: %s", logFile)
	}
}

func createScriptDir() {
	if _, err := os.Stat(scriptDir); os.IsNotExist(err) {
		err := os.MkdirAll(scriptDir, 0755)
		if err != nil {
			log.Fatalf("Failed to create script directory: %v", err)
		}
	}
}

func startTelegraf() {
	cmd := exec.Command(config.TelegrafExecutable, "--config", config.ConfigFile)

	// 启动 Telegraf 进程
	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start Telegraf: %v", err)
	}

	// 获取 Telegraf 可执行文件的绝对路径
	executablePath, err := filepath.Abs(cmd.Path)
	if err != nil {
		log.Printf("Failed to get absolute path of Telegraf executable: %v", err)
		return
	}

	// 获取 Telegraf 可执行文件所在的目录
	executableDir := filepath.Dir(executablePath)

	// 设置进程信息
	info := ProcessInfo{
		PID:       cmd.Process.Pid,
		Directory: executableDir,
		StartTime: time.Now().Format(time.RFC3339),
	}

	// 更新 processInfo
	processInfo.Store(infoKey, info)

	shouldRun = true

	log.Printf("Telegraf started with PID %d\n", cmd.Process.Pid)

	// 将 telegrafCmd 赋值为新启动的命令
	telegrafCmd = cmd

	// 使用 goroutine 监控 Telegraf 进程
	go monitorTelegrafProcess(cmd)
}

func monitorTelegrafProcess(cmd *exec.Cmd) {
	if err := cmd.Wait(); err != nil {
		log.Printf("Telegraf exited with error: %v\n", err)
	} else {
		log.Println("Telegraf exited normally")
	}

	// 清除进程信息
	processInfo.Delete(infoKey)

	// 更新标志变量
	shouldRun = false
	telegrafCmd = nil
}

func stopTelegraf() {
	if telegrafCmd != nil && telegrafCmd.Process != nil {
		if err := telegrafCmd.Process.Kill(); err != nil {
			log.Printf("Failed to kill Telegraf process: %v\n", err)
		} else {
			log.Printf("Telegraf process with PID %d killed\n", telegrafCmd.Process.Pid)
		}
		telegrafCmd = nil
	}
	shouldRun = false
}

func sendHUPSignal(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(syscall.SIGHUP)
}

func isTelegrafRunning() bool {
	if telegrafCmd != nil && telegrafCmd.Process != nil {
		_, err := os.FindProcess(telegrafCmd.Process.Pid)
		if err == nil {
			return true
		}
	}
	return false
}

func startHTTPServer() {
	http.HandleFunc("/info", maybeAuthenticate(handleInfoRequest))
	http.HandleFunc("/start", maybeAuthenticate(handleStartRequest))
	http.HandleFunc("/stop", maybeAuthenticate(handleStopRequest))
	http.HandleFunc("/update-config", maybeAuthenticate(handleUpdateConfigRequest))
	http.HandleFunc("/restore", maybeAuthenticate(handleRestoreRequest))
	http.HandleFunc("/list-backup", maybeAuthenticate(handleListBackupRequest))
	http.HandleFunc("/update-script", maybeAuthenticate(handleUpdateScriptRequest))
	http.HandleFunc("/log", maybeAuthenticate(handleLogRequest)) // 新增日志处理函数
	log.Println("Starting HTTP server on", config.HTTPPort)
	if err := http.ListenAndServe(config.HTTPPort, nil); err != nil {
		log.Fatalf("Failed to start HTTP server: %v", err)
	}
}

const infoKey = "process_info"

func handleInfoRequest(w http.ResponseWriter, r *http.Request) {
	infoInterface, ok := processInfo.Load(infoKey)
	var info ProcessInfo
	if ok {
		info = infoInterface.(ProcessInfo)
	} else {
		info = ProcessInfo{}
	}

	uptime := ""
	if info.PID != 0 {
		startTime, err := time.Parse(time.RFC3339, info.StartTime)
		if err == nil {
			uptime = time.Since(startTime).String()
		}
	}
	info.Uptime = uptime

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

func handleStartRequest(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	defer mutex.Unlock()

	if shouldDaemon {
		sendResponse(w, -1, "Telegraf is already being daemonized")
		return
	}

	shouldDaemon = true

	for !isTelegrafRunning() {
		time.Sleep(1 * time.Second)
		log.Println("is running? ", isTelegrafRunning())
		if isTelegrafRunning() {
			break
		}
	}

	sendResponse(w, 0, "Telegraf daemonization started")
}

func handleStopRequest(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	defer mutex.Unlock()

	if !shouldDaemon {
		sendResponse(w, -1, "Telegraf is not being daemonized")
		return
	}

	shouldDaemon = false
	stopTelegraf()

	sendResponse(w, 0, "Telegraf daemonization stopped")
}

func handleUpdateConfigRequest(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	defer mutex.Unlock()

	if r.Method != http.MethodPost {
		sendResponse(w, -1, "Only POST method is allowed")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		sendResponse(w, -1, "Failed to read request body")
		return
	}
	defer r.Body.Close()

	tempConfigFilePath := config.ConfigFile + ".tmp"
	if err := os.WriteFile(tempConfigFilePath, body, 0644); err != nil {
		sendResponse(w, -1, "Failed to write temporary configuration file")
		return
	}

	// 更新配置并重新加载
	err = updateAndReloadConfig(tempConfigFilePath, config.ConfigFile)
	if err != nil {
		os.Remove(tempConfigFilePath) // 确保临时文件被删除
		sendResponse(w, -1, fmt.Sprintf("Failed to update and reload configuration: %v", err))
		return
	}

	os.Remove(tempConfigFilePath) // 删除临时文件

	// 重新读取日志文件路径
	logFile = getLogFileFromTelegrafConfig(config.ConfigFile)
	if logFile == "" {
		log.Println("Log file path not found in updated Telegraf configuration.")
	} else {
		log.Printf("Updated log file path from Telegraf config: %s", logFile)
	}

	sendResponse(w, 0, "Configuration updated and refreshed successfully")
}

func handleRestoreRequest(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	defer mutex.Unlock()

	if r.Method != http.MethodPost {
		sendResponse(w, -1, "Only POST method is allowed")
		return
	}

	type RestoreRequest struct {
		BackupFile string `json:"backup_file"`
	}
	var req RestoreRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		sendResponse(w, -1, "Invalid JSON format")
		return
	}
	defer r.Body.Close()

	backupFileName := req.BackupFile
	if backupFileName == "" {
		sendResponse(w, -1, "Backup file name is required")
		return
	}

	// 防止路径遍历攻击和非法字符
	if strings.ContainsRune(backupFileName, '/') || strings.ContainsRune(backupFileName, '\\') || strings.HasPrefix(backupFileName, ".") || strings.HasPrefix(backupFileName, " ") || strings.ContainsAny(backupFileName, "<>?*|\":;") {
		sendResponse(w, -1, "Invalid backup file name")
		return
	}

	// 构建备份文件的完整路径
	backupFilePath := filepath.Join(filepath.Dir(config.ConfigFile), backupFileName)

	// 检查备份文件是否存在
	if _, err := os.Stat(backupFilePath); os.IsNotExist(err) {
		sendResponse(w, -1, "Backup file does not exist")
		return
	}

	// 更新配置并重新加载
	err = updateAndReloadConfig(backupFilePath, config.ConfigFile)
	if err != nil {
		sendResponse(w, -1, fmt.Sprintf("Failed to update and reload configuration: %v", err))
		return
	}

	// 重新读取日志文件路径
	logFile = getLogFileFromTelegrafConfig(config.ConfigFile)
	if logFile == "" {
		log.Println("Log file path not found in restored Telegraf configuration.")
	} else {
		log.Printf("Restored log file path from Telegraf config: %s", logFile)
	}

	sendResponse(w, 0, "Configuration restored successfully")
}

func handleListBackupRequest(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	defer mutex.Unlock()

	dir := filepath.Dir(config.ConfigFile)
	files, err := os.ReadDir(dir)
	if err != nil {
		sendResponse(w, -1, fmt.Sprintf("Failed to read directory: %v", err))
		return
	}

	var backups []string
	for _, file := range files {
		if strings.HasSuffix(file.Name(), ".bak") {
			backups = append(backups, file.Name())
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(backups)
}

func handleUpdateScriptRequest(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	defer mutex.Unlock()

	if r.Method != http.MethodPost {
		sendResponse(w, -1, "Only POST method is allowed")
		return
	}

	// 解析 multipart form data
	err := r.ParseMultipartForm(50 << 20) // 50 MB max memory
	if err != nil {
		sendResponse(w, -1, "Failed to parse multipart form data")
		return
	}

	file, handler, err := r.FormFile("file")
	if err != nil {
		sendResponse(w, -1, "Failed to retrieve file from form data")
		return
	}
	defer file.Close()

	// 检查文件扩展名是否为 .zip
	ext := filepath.Ext(handler.Filename)
	if ext != ".zip" {
		sendResponse(w, -1, "Uploaded file must be a zip archive")
		return
	}

	// 创建临时文件以保存上传的 ZIP 文件
	tempZipPath := filepath.Join(os.TempDir(), handler.Filename)
	tempZipFile, err := os.Create(tempZipPath)
	if err != nil {
		sendResponse(w, -1, fmt.Sprintf("Failed to create temporary file: %v", err))
		return
	}
	defer tempZipFile.Close()
	defer os.Remove(tempZipPath) // 确保临时文件被删除

	// 将上传的文件内容写入临时文件
	_, err = io.Copy(tempZipFile, file)
	if err != nil {
		sendResponse(w, -1, fmt.Sprintf("Failed to save uploaded file: %v", err))
		return
	}

	// 打开临时 ZIP 文件
	tempZipFile, err = os.Open(tempZipPath)
	if err != nil {
		sendResponse(w, -1, fmt.Sprintf("Failed to open temporary file: %v", err))
		return
	}
	defer tempZipFile.Close()

	// 获取临时 ZIP 文件的信息以获取其大小
	fileInfo, err := tempZipFile.Stat()
	if err != nil {
		sendResponse(w, -1, fmt.Sprintf("Failed to get file information: %v", err))
		return
	}

	// 清空 script 目录
	err = clearDir(scriptDir)
	if err != nil {
		sendResponse(w, -1, fmt.Sprintf("Failed to clear script directory: %v", err))
		return
	}

	// 解压 ZIP 文件到 script 目录
	err = unzip(tempZipFile, fileInfo.Size(), scriptDir)
	if err != nil {
		sendResponse(w, -1, fmt.Sprintf("Failed to unzip file: %v", err))
		return
	}

	// 给所有 .sh 文件添加可执行权限
	err = addExecPermissionsToShFiles(scriptDir)
	if err != nil {
		sendResponse(w, -1, fmt.Sprintf("Failed to set executable permissions: %v", err))
		return
	}

	sendResponse(w, 0, "Scripts updated successfully")
}

func handleLogRequest(w http.ResponseWriter, r *http.Request) {
	if logFile == "" {
		sendResponse(w, -1, "Log file path not found in Telegraf configuration.")
		return
	}

	linesParam := r.URL.Query().Get("lines")
	if linesParam == "" {
		linesParam = "10" // 默认显示最后10行
	}

	lines, err := strconv.Atoi(linesParam)
	if err != nil || lines <= 0 {
		sendResponse(w, -1, "Invalid 'lines' parameter")
		return
	}

	logLines, err := tailFile(logFile, lines)
	if err != nil {
		sendResponse(w, -1, fmt.Sprintf("Failed to read log file: %v", err))
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(strings.Join(logLines, "\n")))
}

func tailFile(filePath string, n int) ([]string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines list.List

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		lines.PushBack(line)
		if lines.Len() > n {
			e := lines.Front()
			lines.Remove(e)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	var result []string
	for e := lines.Front(); e != nil; e = e.Next() {
		result = append(result, e.Value.(string))
	}

	return result, nil
}

func updateAndReloadConfig(newConfigPath, targetConfigPath string) error {
	// 验证配置文件
	valid, err := validateConfig(newConfigPath)
	if err != nil {
		return fmt.Errorf("failed to validate configuration: %v", err)
	}

	if !valid {
		return fmt.Errorf("configuration is invalid")
	}

	// 备份旧的配置文件
	backupFilePath, err := backupConfigFile(targetConfigPath)
	if err != nil {
		return fmt.Errorf("failed to backup configuration file: %v", err)
	}
	log.Printf("Old configuration backed up to %s\n", backupFilePath)

	// 替换旧的配置文件
	if err := copyFile(newConfigPath, targetConfigPath); err != nil {
		return fmt.Errorf("failed to replace configuration file: %v", err)
	}

	// 如果 Telegraf 正在运行，发送 SIGHUP 信号以热加载配置
	if isTelegrafRunning() {
		if err := sendHUPSignal(telegrafCmd.Process.Pid); err != nil {
			return fmt.Errorf("failed to send HUP signal: %v", err)
		}
		log.Printf("Sent SIGHUP to Telegraf process with PID %d to reload configuration\n", telegrafCmd.Process.Pid)
	} else {
		log.Println("Telegraf is not running, no need to send SIGHUP signal")
	}

	return nil
}

func backupConfigFile(configFile string) (string, error) {
	// 使用当前时间生成备份文件名
	backupFileName := fmt.Sprintf("%s.%s.bak", configFile, time.Now().Format("20060102150405"))
	backupFilePath := filepath.Join(filepath.Dir(configFile), backupFileName)

	// 打开源文件
	srcFile, err := os.Open(configFile)
	if err != nil {
		return "", err
	}
	defer srcFile.Close()

	// 创建目标文件
	dstFile, err := os.Create(backupFilePath)
	if err != nil {
		return "", err
	}
	defer dstFile.Close()

	// 拷贝文件内容
	_, err = io.Copy(dstFile, srcFile)
	if err != nil {
		return "", err
	}

	return backupFilePath, nil
}

func copyFile(src, dst string) error {
	// 打开源文件
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	// 创建目标文件
	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	// 拷贝文件内容
	_, err = io.Copy(dstFile, srcFile)
	if err != nil {
		return err
	}

	return nil
}

func restoreBackupFile(originalConfigFile, backupFileName string) error {
	return copyFile(backupFileName, originalConfigFile)
}

func validateConfig(configFile string) (bool, error) {
	cmd := exec.Command(config.TelegrafExecutable, "--config", configFile, "--test")
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Validation failed: %s\n", output)
		return false, nil
	}
	log.Printf("Validation succeeded: %s\n", output)
	return true, nil
}

func clearDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()

	names, err := d.Readdirnames(-1)
	if err != nil {
		return err
	}

	for _, name := range names {
		err = os.RemoveAll(filepath.Join(dir, name))
		if err != nil {
			return err
		}
	}

	return nil
}

func unzip(src io.ReaderAt, size int64, dest string) error {
	r, err := zip.NewReader(src, size)
	if err != nil {
		return err
	}

	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()

		path := filepath.Join(dest, f.Name)

		if f.FileInfo().IsDir() {
			os.MkdirAll(path, f.Mode())
		} else {
			os.MkdirAll(filepath.Dir(path), f.Mode())
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
			if err != nil {
				return err
			}
			defer f.Close()

			_, err = io.Copy(f, rc)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func addExecPermissionsToShFiles(dir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() && strings.HasSuffix(info.Name(), ".sh") {
			currentPerms := info.Mode()
			newPerms := currentPerms | 0111 // 添加可执行权限
			return os.Chmod(path, newPerms)
		}

		return nil
	})
}

func getLogFileFromTelegrafConfig(configFile string) string {
	data, err := os.ReadFile(configFile)
	if err != nil {
		log.Printf("Failed to read Telegraf config file: %v", err)
		return ""
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	inAgentSection := false

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if line == "[agent]" {
			inAgentSection = true
			continue
		}

		if inAgentSection {
			if strings.HasPrefix(line, "#") {
				continue // 忽略注释
			}

			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				key := strings.TrimSpace(parts[0])
				value := strings.TrimSpace(parts[1])

				// 去掉引号
				if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
					value = value[1 : len(value)-1]
				}

				if key == "logfile" {
					return value
				}
			}

			if strings.HasPrefix(line, "[") {
				inAgentSection = false // 结束 [agent] 部分
			}
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("Failed to scan Telegraf config file: %v", err)
		return ""
	}

	return ""
}

func sendResponse(w http.ResponseWriter, code int, message string) {
	response := map[string]interface{}{
		"code":    code,
		"message": message,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		authParts := strings.Split(authHeader, " ")
		if len(authParts) != 2 || authParts[0] != "Basic" {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		payload, err := base64.StdEncoding.DecodeString(authParts[1])
		if err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		pair := strings.Split(string(payload), ":")
		if len(pair) != 2 || pair[0] != config.Username || pair[1] != config.Password {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	}
}

func maybeAuthenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if config.EnableAuth {
			authenticate(next)(w, r)
		} else {
			next.ServeHTTP(w, r)
		}
	}
}
