package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"io"
	"log"
	"math/big"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/pion/mediadevices/pkg/driver"
	_ "github.com/pion/mediadevices/pkg/driver/camera"
	"github.com/pion/mediadevices/pkg/frame"
	"github.com/pion/mediadevices/pkg/prop"
	"github.com/quic-go/quic-go"
)

var (
	isServer    bool
	serverIP    string
	port        string
	listDevices bool
	camID       int
	targetRes   string
)

const (
	FrameRate      = 5
	JpegQuality    = 50
	GridSize       = 64
	NoiseThreshold = 15
)

func init() {
	flag.BoolVar(&isServer, "server", false, "以服务端模式运行")
	flag.StringVar(&serverIP, "ip", "127.0.0.1", "服务端 IP")
	flag.StringVar(&port, "port", "4242", "UDP 端口")
	flag.BoolVar(&listDevices, "list-devices", false, "列出音视频设备及支持的分辨率")
	flag.IntVar(&camID, "cam", 0, "摄像头设备序号")
	flag.StringVar(&targetRes, "res", "1080", "目标分辨率: 480 / 720 / 1080 / 1440 / 4k")
	flag.Parse()
}

func main() {
	if listDevices {
		printDeviceList()
		os.Exit(0)
	}

	if isServer {
		runServer()
		return
	}
	runClient()
}

// ==========================================
// 设备枚举 (带分辨率详情)
// ==========================================

func printDeviceList() {
	fmt.Println("========== 视频设备 ==========")
	videoDrivers := driver.GetManager().Query(driver.FilterVideoRecorder())
	if len(videoDrivers) == 0 {
		fmt.Println("  (未找到摄像头)")
	}
	for i, d := range videoDrivers {
		info := d.Info()
		fmt.Printf("\n  [摄像头 %d] %s\n", i, info.Label)

		if err := d.Open(); err != nil {
			fmt.Printf("    (无法打开设备查询分辨率: %v)\n", err)
			continue
		}
		props := d.Properties()
		d.Close()

		if len(props) == 0 {
			fmt.Println("    (未报告支持的分辨率)")
			continue
		}

		seen := make(map[string]bool)
		for _, p := range props {
			key := fmt.Sprintf("%dx%d @ %.0ffps (%s)", p.Width, p.Height, p.FrameRate, p.FrameFormat)
			if !seen[key] {
				seen[key] = true
				fmt.Printf("    - %s\n", key)
			}
		}
	}

	fmt.Println("\n========== 音频设备 ==========")
	audioDrivers := driver.GetManager().Query(driver.FilterAudioRecorder())
	if len(audioDrivers) == 0 {
		fmt.Println("  (未找到麦克风)")
	}
	for i, d := range audioDrivers {
		info := d.Info()
		fmt.Printf("  [麦克风 %d] %s\n", i, info.Label)
	}
}

// ==========================================
// 服务端逻辑 (接收端 + 拼图渲染)
// ==========================================

type App struct {
	canvas    *image.RGBA
	canvasW   int
	canvasH   int
	mu        sync.Mutex
	connected bool
}

func (a *App) Update() error { return nil }

func (a *App) Draw(screen *ebiten.Image) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.canvas != nil {
		screen.DrawImage(ebiten.NewImageFromImage(a.canvas), nil)
	}
}

func (a *App) Layout(outsideWidth, outsideHeight int) (screenWidth, screenHeight int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.canvasW > 0 && a.canvasH > 0 {
		return a.canvasW, a.canvasH
	}
	return 640, 480
}

func runServer() {
	fmt.Printf("[Server] 启动 QUIC 监听 UDP 端口 %s...\n", port)
	listener, err := quic.ListenAddr(":"+port, generateTLSConfig(), nil)
	if err != nil {
		log.Fatal(err)
	}

	app := &App{}

	go func() {
		for {
			conn, err := listener.Accept(context.Background())
			if err != nil {
				log.Println("等待连接出错:", err)
				continue
			}
			fmt.Println("[Server] 客户端已连接！")

			go func() {
				stream, err := conn.AcceptStream(context.Background())
				if err != nil {
					return
				}

				var frameW, frameH uint16
				binary.Read(stream, binary.BigEndian, &frameW)
				binary.Read(stream, binary.BigEndian, &frameH)
				fmt.Printf("[Server] 客户端画面尺寸: %dx%d\n", frameW, frameH)

				app.mu.Lock()
				app.canvasW = int(frameW)
				app.canvasH = int(frameH)
				app.canvas = image.NewRGBA(image.Rect(0, 0, int(frameW), int(frameH)))
				app.connected = true
				app.mu.Unlock()

				ebiten.SetWindowSize(int(frameW), int(frameH))

				for {
					var totalSize uint32
					if err := binary.Read(stream, binary.BigEndian, &totalSize); err != nil {
						log.Println("[Server] 连接断开:", err)
						break
					}

					var tileCount uint16
					if err := binary.Read(stream, binary.BigEndian, &tileCount); err != nil {
						break
					}

					for i := 0; i < int(tileCount); i++ {
						var x int16
						var y int16
						var jpegSize uint32
						binary.Read(stream, binary.BigEndian, &x)
						binary.Read(stream, binary.BigEndian, &y)
						binary.Read(stream, binary.BigEndian, &jpegSize)

						jpegData := make([]byte, jpegSize)
						if _, err := io.ReadFull(stream, jpegData); err != nil {
							break
						}

						tileImg, err := jpeg.Decode(bytes.NewReader(jpegData))
						if err != nil {
							continue
						}

						app.mu.Lock()
						targetRect := image.Rect(int(x), int(y), int(x)+tileImg.Bounds().Dx(), int(y)+tileImg.Bounds().Dy())
						draw.Draw(app.canvas, targetRect, tileImg, image.Point{0, 0}, draw.Src)
						app.mu.Unlock()
					}

					if tileCount > 0 {
						fmt.Printf("\r[Server] 收到 %4d 个变化网格", tileCount)
					}
				}
			}()
		}
	}()

	ebiten.SetWindowTitle("极客监控 (QUIC + 网格增量)")
	ebiten.SetWindowResizingMode(ebiten.WindowResizingModeEnabled)
	if err := ebiten.RunGame(app); err != nil {
		log.Fatal(err)
	}
}

// ==========================================
// 客户端逻辑 (摄像头采集 + 网格差异推流)
// ==========================================

func runClient() {
	address := fmt.Sprintf("%s:%s", serverIP, port)
	tlsConf := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"quic-monitor"}}

	fmt.Printf("[Client] 正在连接服务端 %s...\n", address)
	conn, err := quic.DialAddr(context.Background(), address, tlsConf, nil)
	if err != nil {
		log.Fatal("连接失败:", err)
	}

	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		log.Fatal("开启 Stream 失败:", err)
	}

	// 1. 查找并打开摄像头
	videoDrivers := driver.GetManager().Query(driver.FilterVideoRecorder())
	if len(videoDrivers) == 0 {
		log.Fatal("未找到任何摄像头设备")
	}
	if camID >= len(videoDrivers) {
		log.Fatalf("摄像头序号 %d 不存在，最大为 %d", camID, len(videoDrivers)-1)
	}

	cam := videoDrivers[camID]
	fmt.Printf("[Client] 正在打开摄像头: %s\n", cam.Info().Label)

	if err := cam.Open(); err != nil {
		log.Fatalf("打开摄像头失败: %v", err)
	}
	defer cam.Close()

	recorder, ok := cam.(driver.VideoRecorder)
	if !ok {
		log.Fatal("该设备不支持视频录制")
	}

	// 2. 智能选择最佳分辨率
	targetW, targetH := parseTargetRes(targetRes)
	selectedProp := selectBestProp(cam.Properties(), targetW, targetH)
	fmt.Printf("[Client] 摄像头实际输出: %dx%d @ %.0f fps\n", selectedProp.Width, selectedProp.Height, selectedProp.FrameRate)

	// 3. 启动采集
	reader, err := recorder.VideoRecord(selectedProp)
	if err != nil {
		log.Fatalf("启动视频采集失败: %v", err)
	}

	actualW := selectedProp.Width
	actualH := selectedProp.Height
	fmt.Printf("[Client] 摄像头启动成功，目标 %dx%d，推流中 (%d FPS)...\n", actualW, actualH, FrameRate)

	// 4. 先发送画面尺寸给服务端
	binary.Write(stream, binary.BigEndian, uint16(actualW))
	binary.Write(stream, binary.BigEndian, uint16(actualH))

	var prevImage *image.RGBA
	frameBuffer := new(bytes.Buffer)
	tileBuffer := new(bytes.Buffer)
	ticker := time.NewTicker(time.Second / time.Duration(FrameRate))
	defer ticker.Stop()

	for range ticker.C {
		rawImg, release, err := reader.Read()
		if err != nil {
			log.Println("读取帧失败:", err)
			continue
		}

		// 5. 转换为 RGBA
		bounds := rawImg.Bounds()
		currImage := toRGBA(rawImg)
		release()

		width := bounds.Dx()
		height := bounds.Dy()

		frameBuffer.Reset()
		var dirtyTilesCount uint16
		var totalJpegBytes int

		// 6. 网格差异检测
		for y := 0; y < height; y += GridSize {
			for x := 0; x < width; x += GridSize {
				tileRect := image.Rect(x, y, minInt(x+GridSize, width), minInt(y+GridSize, height))

				if isTileChanged(currImage, prevImage, tileRect) {
					dirtyTilesCount++

					tileImg := currImage.SubImage(tileRect)
					tileBuffer.Reset()
					jpeg.Encode(tileBuffer, tileImg, &jpeg.Options{Quality: JpegQuality})

					jpegData := tileBuffer.Bytes()
					totalJpegBytes += len(jpegData)

					binary.Write(frameBuffer, binary.BigEndian, int16(x))
					binary.Write(frameBuffer, binary.BigEndian, int16(y))
					binary.Write(frameBuffer, binary.BigEndian, uint32(len(jpegData)))
					frameBuffer.Write(jpegData)
				}
			}
		}

		payloadData := frameBuffer.Bytes()
		totalSize := uint32(2 + len(payloadData))

		binary.Write(stream, binary.BigEndian, totalSize)
		binary.Write(stream, binary.BigEndian, dirtyTilesCount)
		stream.Write(payloadData)

		fmt.Printf("\r[+] 帧 | 变化网格: %4d | 流量: %4d KB   ", dirtyTilesCount, totalJpegBytes/1024)
		prevImage = currImage
	}
}

// ==========================================
// 分辨率选择
// ==========================================

func parseTargetRes(res string) (int, int) {
	switch res {
	case "480":
		return 640, 480
	case "720":
		return 1280, 720
	case "1080":
		return 1920, 1080
	case "1440":
		return 2560, 1440
	case "4k":
		return 3840, 2160
	default:
		return 1920, 1080
	}
}

func selectBestProp(props []prop.Media, targetW, targetH int) prop.Media {
	if len(props) == 0 {
		p := prop.Media{}
		p.Width = targetW
		p.Height = targetH
		p.FrameRate = float32(FrameRate)
		return p
	}

	fmt.Println("[Client] 摄像头支持的分辨率:")
	seen := make(map[string]bool)
	for _, p := range props {
		key := fmt.Sprintf("  - %dx%d @ %.0ffps (格式: %s)", p.Width, p.Height, p.FrameRate, p.FrameFormat)
		if !seen[key] {
			seen[key] = true
			fmt.Println(key)
		}
	}

	var valid []prop.Media
	for _, p := range props {
		if p.Width > 0 && p.Height > 0 {
			valid = append(valid, p)
		}
	}
	if len(valid) == 0 {
		return props[0]
	}

	sort.Slice(valid, func(i, j int) bool {
		diffI := absInt(valid[i].Width-targetW) + absInt(valid[i].Height-targetH)
		diffJ := absInt(valid[j].Width-targetW) + absInt(valid[j].Height-targetH)
		return diffI < diffJ
	})

	best := valid[0]

	for _, p := range valid {
		diff := absInt(p.Width-targetW) + absInt(p.Height-targetH)
		bestDiff := absInt(best.Width-targetW) + absInt(best.Height-targetH)
		if diff == bestDiff {
			if p.FrameFormat == frame.FormatMJPEG || p.FrameFormat == frame.FormatNV21 || p.FrameFormat == frame.FormatYUY2 {
				best = p
				break
			}
		}
	}

	return best
}

// ==========================================
// 图像格式转换
// ==========================================

func toRGBA(img image.Image) *image.RGBA {
	if rgba, ok := img.(*image.RGBA); ok {
		return rgba
	}
	bounds := img.Bounds()
	rgba := image.NewRGBA(bounds)
	draw.Draw(rgba, bounds, img, bounds.Min, draw.Src)
	return rgba
}

// ==========================================
// 网格差异检测
// ==========================================

func isTileChanged(curr, prev *image.RGBA, rect image.Rectangle) bool {
	if prev == nil {
		return true
	}

	currBounds := curr.Bounds()
	prevBounds := prev.Bounds()
	if rect.Max.X > currBounds.Max.X || rect.Max.Y > currBounds.Max.Y {
		return true
	}
	if rect.Max.X > prevBounds.Max.X || rect.Max.Y > prevBounds.Max.Y {
		return true
	}

	var diffSum, count int64
	for y := rect.Min.Y; y < rect.Max.Y; y += 4 {
		for x := rect.Min.X; x < rect.Max.X; x += 4 {
			i := curr.PixOffset(x, y)
			j := prev.PixOffset(x, y)

			if i+2 >= len(curr.Pix) || j+2 >= len(prev.Pix) {
				return true
			}

			dr := int(curr.Pix[i]) - int(prev.Pix[j])
			dg := int(curr.Pix[i+1]) - int(prev.Pix[j+1])
			db := int(curr.Pix[i+2]) - int(prev.Pix[j+2])
			if dr < 0 {
				dr = -dr
			}
			if dg < 0 {
				dg = -dg
			}
			if db < 0 {
				db = -db
			}
			diffSum += int64(dr + dg + db)
			count++
		}
	}
	if count == 0 {
		return false
	}
	return int(diffSum/(count*3)) > NoiseThreshold
}

// ==========================================
// 工具函数
// ==========================================

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func absInt(a int) int {
	if a < 0 {
		return -a
	}
	return a
}

func generateTLSConfig() *tls.Config {
	key, _ := rsa.GenerateKey(rand.Reader, 1024)
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour * 24 * 365),
	}
	certDER, _ := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	tlsCert := tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  key,
	}
	return &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		NextProtos:   []string{"quic-monitor"},
	}
}
