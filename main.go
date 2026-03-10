package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
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
	"github.com/pion/mediadevices"
	_ "github.com/pion/mediadevices/pkg/driver/camera" // 必须匿名引入以注册摄像头驱动
	"github.com/pion/mediadevices/pkg/prop"
	"github.com/quic-go/quic-go"
)

var (
	isServer    bool
	serverIP    string
	port        string
	listDevices bool
	camLabel    string // Pion 通过设备 Label 来选择
)

const (
	FrameRate      = 5
	JpegQuality    = 50
	GridSize       = 64
	NoiseThreshold = 15 // 摄像头噪点大，阈值调高一点防抖动
)

func init() {
	flag.BoolVar(&isServer, "server", false, "以服务端模式运行")
	flag.StringVar(&serverIP, "ip", "127.0.0.1", "服务端 IP")
	flag.StringVar(&port, "port", "4242", "UDP 端口")
	flag.BoolVar(&listDevices, "list-devices", false, "列出音视频设备")
	flag.StringVar(&camLabel, "cam", "", "指定摄像头名称 (留空则默认第一个)")
	flag.Parse()
}

func main() {
	if listDevices {
		fmt.Println("[-] 正在扫描物理设备...")
		devices := mediadevices.EnumerateDevices()
		for _, d := range devices {
			if d.DeviceType == mediadevices.VideoInput {
				fmt.Printf("    [摄像头] ID: %s | 名称: %s\n", d.DeviceID, d.Label)
			}
			if d.DeviceType == mediadevices.AudioInput {
				fmt.Printf("    [麦克风] ID: %s | 名称: %s\n", d.DeviceID, d.Label)
			}
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
// 服务端逻辑 (接收端 + 渲染) 保持原样
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
	if a.canvas != nil { return a.canvas.Bounds().Dx(), a.canvas.Bounds().Dy() }
	return 640, 480
}

func runServer() {
	fmt.Printf("[Server] 启动 QUIC 监听 UDP 端口 %s...\n", port)
	listener, err := quic.ListenAddr(":"+port, generateTLSConfig(), nil)
	if err != nil { log.Fatal(err) }

	app := &App{}

	go func() {
		conn, err := listener.Accept(context.Background())
		if err != nil { return }
		fmt.Println("[Server] 客户端已连接！")

		stream, err := conn.AcceptStream(context.Background())
		if err != nil { return }

		for {
			var totalSize uint32
			if err := binary.Read(stream, binary.BigEndian, &totalSize); err != nil { break }
			var tileCount uint16
			binary.Read(stream, binary.BigEndian, &tileCount)

			for i := 0; i < int(tileCount); i++ {
				var x, y int16
				var jpegSize uint32
				binary.Read(stream, binary.BigEndian, &x, &y, &jpegSize)

				jpegData := make([]byte, jpegSize)
				io.ReadFull(stream, jpegData)

				tileImg, err := jpeg.Decode(bytes.NewReader(jpegData))
				if err != nil { continue }

				app.mu.Lock()
				if app.canvas == nil { app.canvas = image.NewRGBA(image.Rect(0, 0, 1920, 1080)) }
				targetRect := image.Rect(int(x), int(y), int(x)+tileImg.Bounds().Dx(), int(y)+tileImg.Bounds().Dy())
				draw.Draw(app.canvas, targetRect, tileImg, image.Point{0, 0}, draw.Src)
				app.mu.Unlock()
			}
		}
	}()

	ebiten.SetWindowTitle("极客监控 (真实摄像头)")
	ebiten.SetWindowResizingMode(ebiten.WindowResizingModeEnabled)
	if err := ebiten.RunGame(app); err != nil { log.Fatal(err) }
}

// ==========================================
// 客户端逻辑 (Pion 摄像头采集)
// ==========================================

func runClient() {
	address := fmt.Sprintf("%s:%s", serverIP, port)
	tlsConf := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"quic-monitor"}}
	conn, err := quic.DialAddr(context.Background(), address, tlsConf, nil)
	if err != nil { log.Fatal("连接失败:", err) }

	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil { log.Fatal("开启 Stream 失败:", err) }

	// 配置摄像头参数 (强制 640x480, 5fps)
	mediaStream, err := mediadevices.GetUserMedia(mediadevices.MediaStreamConstraints{
		Video: func(c *mediadevices.VideoTrackConstraints) {
			c.Width = prop.Int(640)
			c.Height = prop.Int(480)
			c.FrameRate = prop.Float(FrameRate)
			if camLabel != "" {
				// c.DeviceID = prop.String(camLabel) // 实际应用中可通过 Label 查 ID
			}
		},
	})
	if err != nil { log.Fatalf("打开摄像头失败: %v", err) }

	videoTrack := mediaStream.GetVideoTracks()[0]
	videoReader := videoTrack.(*mediadevices.VideoTrack).NewReader(false)
	fmt.Printf("[Client] 物理摄像头启动成功，网格抗噪推流中...\n")

	var prevImage *image.RGBA
	frameBuffer := new(bytes.Buffer)
	tileBuffer := new(bytes.Buffer)

	for {
		// 从物理摄像头读取一帧
		img, release, err := videoReader.Read()
		if err != nil { continue }

		bounds := img.Bounds()
		width, height := bounds.Dx(), bounds.Dy()
		
		// 转换为 RGBA 以便统一处理
		currImage := image.NewRGBA(bounds)
		draw.Draw(currImage, bounds, img, image.Point{0, 0}, draw.Src)
		release() // 必须释放底层的 C 内存！

		frameBuffer.Reset()
		var dirtyTilesCount uint16
		var totalJpegBytes int

		for y := 0; y < height; y += GridSize {
			for x := 0; x < width; x += GridSize {
				tileRect := image.Rect(x, y, min(x+GridSize, width), min(y+GridSize, height))
				
				if isTileChanged(currImage, prevImage, tileRect) {
					dirtyTilesCount++
					
					tileImg := currImage.SubImage(tileRect)
					tileBuffer.Reset()
					jpeg.Encode(tileBuffer, tileImg, &jpeg.Options{Quality: JpegQuality})

					jpegData := tileBuffer.Bytes()
					jpegLen := uint32(len(jpegData))
					totalJpegBytes += len(jpegData)

					binary.Write(frameBuffer, binary.BigEndian, int16(x), int16(y), jpegLen)
					frameBuffer.Write(jpegData)
				}
			}
		}

		payloadData := frameBuffer.Bytes()
		totalSize := uint32(2 + len(payloadData))

		binary.Write(stream, binary.BigEndian, totalSize)
		binary.Write(stream, binary.BigEndian, dirtyTilesCount)
		stream.Write(payloadData)

		fmt.Printf("\r[+] 发送帧 | 变化网格: %4d 个 | 流量: %4d KB   ", dirtyTilesCount, totalJpegBytes/1024)
		prevImage = currImage
	}
}

func isTileChanged(curr, prev *image.RGBA, rect image.Rectangle) bool {
	if prev == nil { return true }
	var diffSum, count int64
	for y := rect.Min.Y; y < rect.Max.Y; y += 4 {
		for x := rect.Min.X; x < rect.Max.X; x += 4 {
			i := curr.PixOffset(x, y)
			j := prev.PixOffset(x, y)
			dr, dg, db := int(curr.Pix[i])-int(prev.Pix[j]), int(curr.Pix[i+1])-int(prev.Pix[j+1]), int(curr.Pix[i+2])-int(prev.Pix[j+2])
			if dr < 0 { dr = -dr }
			if dg < 0 { dg = -dg }
			if db < 0 { db = -db }
			diffSum += int64(dr + dg + db)
			count++
		}
	}
	if count == 0 { return false }
	return int(diffSum/(count*3)) > NoiseThreshold
}

func min(a, b int) int { if a < b { return a } ; return b }
func generateTLSConfig() *tls.Config {
	key, _ := rsa.GenerateKey(rand.Reader, 1024)
	template := x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour * 24)}
	certDER, _ := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{certDER}, PrivateKey: key}}, NextProtos: []string{"quic-monitor"}}
}
