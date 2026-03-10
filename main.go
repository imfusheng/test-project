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

	"github.com/gen2brain/cam"
	"github.com/hajimehoshi/ebiten/v2"
	"github.com/quic-go/quic-go"
)

// ==========================================
// 全局变量与配置
// ==========================================
var (
	isServer    bool
	serverIP    string
	port        string
	listDevices bool
	camID       int
)

const (
	FrameRate      = 5  // 目标帧率 5fps
	JpegQuality    = 50 // JPEG 质量
	GridSize       = 64 // 网格切割大小 (64x64像素)
	NoiseThreshold = 12 // 噪点容忍度 (0-255，越大越不灵敏，过滤摄像头噪点)
)

func init() {
	flag.BoolVar(&isServer, "server", false, "以服务端(接收/播放)模式运行")
	flag.StringVar(&serverIP, "ip", "127.0.0.1", "服务端 IP")
	flag.StringVar(&port, "port", "4242", "UDP 端口")
	flag.BoolVar(&listDevices, "list-devices", false, "列出本地设备")
	flag.IntVar(&camID, "cam", 0, "使用的摄像头 ID")
	flag.Parse()
}

func main() {
	if listDevices {
		fmt.Println("[-] 正在扫描本地视频设备...")
		fmt.Println("    [0] 默认摄像头")
		fmt.Println("    [1] 外接摄像头 (如果有)")
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
	canvas *image.RGBA // 持久化画布，收到变化的小方块就贴上去
	mu     sync.Mutex
}

func (a *App) Update() error { return nil }

func (a *App) Draw(screen *ebiten.Image) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.canvas != nil {
		img := ebiten.NewImageFromImage(a.canvas)
		screen.DrawImage(img, nil)
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
			// 1. 读取整个数据包的总长度
			var totalSize uint32
			if err := binary.Read(stream, binary.BigEndian, &totalSize); err != nil {
				log.Println("连接断开:", err)
				break
			}

			// 2. 读取变动的方块数量
			var tileCount uint16
			binary.Read(stream, binary.BigEndian, &tileCount)

			// 初始化或重绘画布尺寸 (假设前几个包包含分辨率信息，为简化直接用首帧动态创建)
			for i := 0; i < int(tileCount); i++ {
				var x, y int16
				var jpegSize uint32
				binary.Read(stream, binary.BigEndian, &x)
				binary.Read(stream, binary.BigEndian, &y)
				binary.Read(stream, binary.BigEndian, &jpegSize)

				jpegData := make([]byte, jpegSize)
				io.ReadFull(stream, jpegData)

				tileImg, err := jpeg.Decode(bytes.NewReader(jpegData))
				if err != nil {
					continue
				}

				app.mu.Lock()
				// 第一次收到画面，初始化画布
				if app.canvas == nil {
					// 暂时预估一个 1920x1080 的极大底板，实际可动态扩展
					app.canvas = image.NewRGBA(image.Rect(0, 0, 1920, 1080)) 
				}
				// 核心：把收到的解码方块，贴到画布对应的 X, Y 坐标上
				targetRect := image.Rect(int(x), int(y), int(x)+tileImg.Bounds().Dx(), int(y)+tileImg.Bounds().Dy())
				draw.Draw(app.canvas, targetRect, tileImg, image.Point{0, 0}, draw.Src)
				app.mu.Unlock()
			}
		}
	}()

	ebiten.SetWindowTitle("纯 Go 网格差异化监控终端")
	ebiten.SetWindowResizingMode(ebiten.WindowResizingModeEnabled)
	if err := ebiten.RunGame(app); err != nil {
		log.Fatal(err)
	}
}

// ==========================================
// 客户端逻辑 (推流端 + 差异分析)
// ==========================================

// SubImager 接口用于从大图中裁剪小方块
type SubImager interface {
	SubImage(r image.Rectangle) image.Image
}

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

	c, err := cam.New(camID)
	if err != nil {
		log.Fatalf("打开摄像头失败: %v", err)
	}
	defer c.Close()
	fmt.Printf("[Client] 摄像头启动成功，网格抗噪推流中 (%d FPS)...\n", FrameRate)

	ticker := time.NewTicker(time.Second / time.Duration(FrameRate))
	defer ticker.Stop()

	var prevImage image.Image
	frameBuffer := new(bytes.Buffer)
	tileBuffer := new(bytes.Buffer)

	for range ticker.C {
		currImage, err := c.QueryImage()
		if err != nil {
			continue
		}

		bounds := currImage.Bounds()
		width, height := bounds.Max.X, bounds.Max.Y
		subImager, ok := currImage.(SubImager)
		if !ok {
			log.Println("当前摄像头图像格式不支持裁剪")
			continue
		}

		frameBuffer.Reset()
		var dirtyTilesCount uint16
		var totalJpegBytes int

		// 1. 将画面分割成 GridSize * GridSize 的小方块
		for y := 0; y < height; y += GridSize {
			for x := 0; x < width; x += GridSize {
				tileRect := image.Rect(x, y, min(x+GridSize, width), min(y+GridSize, height))
				
				// 2. 判断该方块是否发生了明显变化
				if isTileChanged(currImage, prevImage, tileRect) {
					dirtyTilesCount++
					
					// 3. 裁剪并压缩这个发生变化的小方块
					tileImg := subImager.SubImage(tileRect)
					tileBuffer.Reset()
					jpeg.Encode(tileBuffer, tileImg, &jpeg.Options{Quality: JpegQuality})

					jpegData := tileBuffer.Bytes()
					jpegLen := uint32(len(jpegData))
					totalJpegBytes += len(jpegData)

					// 4. 将方块的元数据和图片数据写入帧缓冲
					binary.Write(frameBuffer, binary.BigEndian, int16(x))
					binary.Write(frameBuffer, binary.BigEndian, int16(y))
					binary.Write(frameBuffer, binary.BigEndian, jpegLen)
					frameBuffer.Write(jpegData)
				}
			}
		}

		// 5. 封包发送给服务端 (帧总长度 + 方块数量 + 方块数据)
		payloadData := frameBuffer.Bytes()
		totalSize := uint32(2 + len(payloadData)) // 2 bytes for tileCount

		binary.Write(stream, binary.BigEndian, totalSize)
		binary.Write(stream, binary.BigEndian, dirtyTilesCount)
		stream.Write(payloadData)

		fmt.Printf("\r[+] 发送帧 | 变化网格: %d 个 | 流量: %d KB   ", dirtyTilesCount, totalJpegBytes/1024)
		prevImage = currImage // 更新历史帧
	}
}

// 核心魔法：抗噪网格比对算法 (算出两个图块的平均像素差异)
func isTileChanged(curr, prev image.Image, rect image.Rectangle) bool {
	if prev == nil {
		return true // 第一帧，或者分辨率改变，全部判定为脏网格
	}
	
	var diffSum int64
	var count int64

	// 步长设为 2 (跳跃采样)，极大提升对比速度，降低 CPU 消耗
	for y := rect.Min.Y; y < rect.Max.Y; y += 2 {
		for x := rect.Min.X; x < rect.Max.X; x += 2 {
			r1, g1, b1, _ := curr.At(x, y).RGBA()
			r2, g2, b2, _ := prev.At(x, y).RGBA()

			// RGBA() 返回 0-65535, 右移 8 位转成 0-255
			dr := int(r1>>8) - int(r2>>8)
			dg := int(g1>>8) - int(g2>>8)
			db := int(b1>>8) - int(b2>>8)

			if dr < 0 { dr = -dr }
			if dg < 0 { dg = -dg }
			if db < 0 { db = -db }

			diffSum += int64(dr + dg + db)
			count++
		}
	}

	if count == 0 { return false }
	avgDiff := int(diffSum / (count * 3))
	
	// 如果平均像素变化超过阈值，才认为画面动了
	return avgDiff > NoiseThreshold 
}

// 辅助函数
func min(a, b int) int {
	if a < b { return a }
	return b
}

func generateTLSConfig() *tls.Config {
	key, _ := rsa.GenerateKey(rand.Reader, 1024)
	template := x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour * 24)}
	certDER, _ := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	tlsCert := tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: key}
	return &tls.Config{Certificates: []tls.Certificate{tlsCert}, NextProtos: []string{"quic-monitor"}}
}
