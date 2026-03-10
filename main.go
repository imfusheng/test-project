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
	"sync"
	"time"

	"github.com/hajimehoshi/ebiten/v2"
	"github.com/pion/mediadevices/pkg/driver"
	_ "github.com/pion/mediadevices/pkg/driver/camera"
	"github.com/pion/mediadevices/pkg/prop"
	"github.com/quic-go/quic-go"
)

var (
	isServer    bool
	serverIP    string
	port        string
	listDevices bool
	camID       int
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
	flag.BoolVar(&listDevices, "list-devices", false, "列出音视频设备")
	flag.IntVar(&camID, "cam", 0, "摄像头设备序号")
	flag.Parse()
}

func main() {
	if listDevices {
		fmt.Println("[-] 正在扫描物理设备...")

		videoDrivers := driver.GetManager().Query(driver.FilterVideoRecorder())
		if len(videoDrivers) == 0 {
			fmt.Println("    (未找到摄像头)")
		}
		for i, d := range videoDrivers {
			info := d.Info()
			fmt.Printf("    [摄像头 %d] %s\n", i, info.Label)
		}

		audioDrivers := driver.GetManager().Query(driver.FilterAudioRecorder())
		if len(audioDrivers) == 0 {
			fmt.Println("    (未找到麦克风)")
		}
		for i, d := range audioDrivers {
			info := d.Info()
			fmt.Printf("    [麦克风 %d] %s\n", i, info.Label)
		}
		os.Exit(0)
	}

	if isServer {
		runServer()
		return
	}
	runClient()
}

// ==========================================
// 服务端逻辑 (接收端 + 拼图渲染)
// ==========================================

type App struct {
	canvas *image.RGBA
	mu     sync.Mutex
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
	if a.canvas != nil {
		return a.canvas.Bounds().Dx(), a.canvas.Bounds().Dy()
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
		conn, err := listener.Accept(context.Background())
		if err != nil {
			return
		}
		fmt.Println("[Server] 客户端已连接！")

		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			return
		}

		for {
			// 读取帧总长度
			var totalSize uint32
			if err := binary.Read(stream, binary.BigEndian, &totalSize); err != nil {
				log.Println("连接断开:", err)
				break
			}

			// 读取变化方块数量
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
				if app.canvas == nil {
					app.canvas = image.NewRGBA(image.Rect(0, 0, 1920, 1080))
				}
				targetRect := image.Rect(int(x), int(y), int(x)+tileImg.Bounds().Dx(), int(y)+tileImg.Bounds().Dy())
				draw.Draw(app.canvas, targetRect, tileImg, image.Point{0, 0}, draw.Src)
				app.mu.Unlock()
			}
		}
	}()

	ebiten.SetWindowTitle("极客监控 (QUIC + 网格增量)")
	ebiten.SetWindowResizingMode(ebiten.WindowResizingModeEnabled)
	if err := ebiten.RunGame(app); err != nil {
		log.Fatal(err)
	}
}

// ==========================================
// 客户端逻辑 (底层 driver 采集 + 网格差异推流)
// ==========================================

func runClient() {
	address := fmt.Sprintf("%s:%s", serverIP, port)
	tlsConf := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"quic-monitor"}}
	conn, err := quic.DialAddr(context.Background(), address, tlsConf, nil)
	if err != nil {
		log.Fatal("连接失败:", err)
	}

	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		log.Fatal("开启 Stream 失败:", err)
	}

	// 1. 查找摄像头驱动
	videoDrivers := driver.GetManager().Query(driver.FilterVideoRecorder())
	if len(videoDrivers) == 0 {
		log.Fatal("未找到任何摄像头设备")
	}
	if camID >= len(videoDrivers) {
		log.Fatalf("摄像头序号 %d 不存在，最大为 %d", camID, len(videoDrivers)-1)
	}

	cam := videoDrivers[camID]
	fmt.Printf("[Client] 正在打开摄像头: %s\n", cam.Info().Label)

	// 2. 打开摄像头
	if err := cam.Open(); err != nil {
		log.Fatalf("打开摄像头失败: %v", err)
	}
	defer cam.Close()

	// 3. 获取摄像头支持的属性，选择最合适的一个
	recorder, ok := cam.(driver.VideoRecorder)
	if !ok {
		log.Fatal("该设备不支持视频录制")
	}

	selectedProp := selectBestProp(cam.Properties(), 640, 480)
	fmt.Printf("[Client] 采集参数: %dx%d\n", selectedProp.Width, selectedProp.Height)

	// 4. 启动视频采集，获得帧读取器
	reader, err := recorder.VideoRecord(selectedProp)
	if err != nil {
		log.Fatalf("启动视频采集失败: %v", err)
	}

	fmt.Printf("[Client] 摄像头启动成功，网格抗噪推流中 (%d FPS)...\n", FrameRate)

	var prevImage *image.RGBA
	frameBuffer := new(bytes.Buffer)
	tileBuffer := new(bytes.Buffer)
	ticker := time.NewTicker(time.Second / time.Duration(FrameRate))
	defer ticker.Stop()

	for range ticker.C {
		// 5. 从摄像头读取一帧原始画面
		img, release, err := reader.Read()
		if err != nil {
			log.Println("读取帧失败:", err)
			continue
		}

		// 6. 转换为 RGBA 以便统一处理
		bounds := img.Bounds()
		currImage := image.NewRGBA(bounds)
		draw.Draw(currImage, bounds, img, bounds.Min, draw.Src)
		release() // 释放底层 C 内存

		width := bounds.Dx()
		height := bounds.Dy()

		frameBuffer.Reset()
		var dirtyTilesCount uint16
		var totalJpegBytes int

		// 7. 核心：网格差异检测
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

		// 8. 封包发送
		payloadData := frameBuffer.Bytes()
		totalSize := uint32(2 + len(payloadData))

		binary.Write(stream, binary.BigEndian, totalSize)
		binary.Write(stream, binary.BigEndian, dirtyTilesCount)
		stream.Write(payloadData)

		fmt.Printf("\r[+] 发送帧 | 变化网格: %4d 个 | 流量: %4d KB   ", dirtyTilesCount, totalJpegBytes/1024)
		prevImage = currImage
	}
}

// 从摄像头支持的属性列表中选择最接近目标分辨率的配置
func selectBestProp(props []prop.Media, targetW, targetH int) prop.Media {
	if len(props) == 0 {
		// 如果摄像头没返回属性列表，手动构造一个默认请求
		p := prop.Media{}
		p.Width = targetW
		p.Height = targetH
		p.FrameRate = float32(FrameRate)
		return p
	}

	best := props[0]
	bestDiff := abs(best.Width-targetW) + abs(best.Height-targetH)

	for _, p := range props[1:] {
		diff := abs(p.Width-targetW) + abs(p.Height-targetH)
		if diff < bestDiff {
			best = p
			bestDiff = diff
		}
	}

	// 覆盖帧率为我们期望的值
	best.FrameRate = float32(FrameRate)
	return best
}

// ==========================================
// 网格差异检测 (跳跃采样 + 抗噪)
// ==========================================

func isTileChanged(curr, prev *image.RGBA, rect image.Rectangle) bool {
	if prev == nil {
		return true
	}
	var diffSum, count int64
	for y := rect.Min.Y; y < rect.Max.Y; y += 4 {
		for x := rect.Min.X; x < rect.Max.X; x += 4 {
			i := curr.PixOffset(x, y)
			j := prev.PixOffset(x, y)
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

func abs(a int) int {
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
