package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waProto "google.golang.org/protobuf/proto"
)

var (
	client        *whatsmeow.Client
	userState     = make(map[string]string)
	ownerStatus   = "offline"
	lastOwnerOnline *time.Time
	ownerJid      = os.Getenv("OWNER_JID")
)

func main() {
	// Setup database untuk simpan sesi
	db, err := sqlstore.New("sqlite", "file:whatsapp.db?_foreign_keys=on", nil)
	if err != nil {
		panic(err)
	}

	// Setup client WhatsApp
	deviceStore, err := db.GetFirstDevice()
	if err != nil {
		panic(err)
	}

	client = whatsmeow.NewClient(deviceStore, nil)
	if client.Store.ID == nil {
		// Belum login, tampilkan QR
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			panic(err)
		}
		for evt := range qrChan {
			if evt.Event == "code" {
				fmt.Println("Scan this QR code:")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else if evt.Event == "success" {
				fmt.Println("Connected to WhatsApp")
				break
			}
		}
	} else {
		// Sudah login, langsung connect
		err = client.Connect()
		if err != nil {
			panic(err)
		}
		fmt.Println("Connected to WhatsApp")
	}

	// Setup Fiber untuk endpoint /health
	app := fiber.New()
	app.Get("/health", func(c *fiber.Ctx) error {
		return c.SendString("Bot is running")
	})
	go app.Listen(":3000")

	// Handler event WhatsApp
	client.AddEventHandler(eventHandler)

	// Loop utama
	select {}
}

func eventHandler(evt interface{}) {
	switch v := evt.(type) {
	case *events.Presence:
		// Deteksi status owner
		if v.From.String() == ownerJid {
			if v.Unavailable {
				ownerStatus = "offline"
			} else {
				ownerStatus = "online"
				now := time.Now()
				lastOwnerOnline = &now
			}
			fmt.Printf("Owner status: %s\n", ownerStatus)
		}

	case *events.Message:
		// Handle pesan
		from := v.Info.Chat
		sender := v.Info.Sender.String()
		isGroup := from.String()[len(from.String())-5:] == "@g.us"
		text := v.Message.GetConversation()

		// Hitung durasi offline
		var offlineDuration float64
		if lastOwnerOnline != nil {
			offlineDuration = time.Since(*lastOwnerOnline).Hours()
		} else {
			offlineDuration = float64(1<<63 - 1) // Infinity
		}
		offlineMessage := ""
		if ownerStatus == "offline" && offlineDuration > 3 {
			offlineMessage = "\n⚠️ Owner sudah offline lebih dari 3 jam, mohon bersabar."
		}

		// Cek apakah bot dimention di grup
		if isGroup {
			mentioned := v.Message.GetExtendedTextMessage().GetContextInfo().GetMentionedJid()
			isMentioned := false
			for _, jid := range mentioned {
				if jid == client.Store.ID.String() {
					isMentioned = true
					break
				}
			}
			if !isMentioned {
				return
			}

			replyText := "👤 Owner sedang online.\n📋 Menu:\n1. Buat stiker"
			if ownerStatus == "offline" {
				replyText = fmt.Sprintf("👤 Owner sedang offline. Silakan tunggu ya~%s", offlineMessage)
			}
			_, err := client.SendMessage(context.Background(), from, &proto.Message{
				Conversation: waProto.String(replyText),
			})
			if err != nil {
				fmt.Printf("Error sending message: %v\n", err)
			}
			userState[sender] = "awaiting_menu"
		} else {
			replyText := "👤 Owner sedang online.\n📋 Menu:\n1. Buat stiker"
			if ownerStatus == "offline" {
				replyText = fmt.Sprintf("👤 Owner sedang offline. Silakan tunggu ya~%s", offlineMessage)
			}
			_, err := client.SendMessage(context.Background(), from, &proto.Message{
				Conversation: waProto.String(replyText),
			})
			if err != nil {
				fmt.Printf("Error sending message: %v\n", err)
			}
			userState[sender] = "awaiting_menu"
		}

		// Cek menu
		if userState[sender] == "awaiting_menu" && text == "1" {
			_, err := client.SendMessage(context.Background(), from, &proto.Message{
				Conversation: waProto.String("📸 Silakan kirim foto untuk diubah jadi stiker"),
			})
			if err != nil {
				fmt.Printf("Error sending message: %v\n", err)
			}
			userState[sender] = "awaiting_image"
			return
		}

		// Cek gambar untuk stiker
		if userState[sender] == "awaiting_image" && v.Message.GetImageMessage() != nil {
			img := v.Message.GetImageMessage()
			url := img.GetUrl()
			resp, err := http.Get(url)
			if err != nil {
				fmt.Printf("Error downloading image: %v\n", err)
				return
			}
			defer resp.Body.Close()

			// Simpan gambar
			imgData, err := io.ReadAll(resp.Body)
			if err != nil {
				fmt.Printf("Error reading image: %v\n", err)
				return
			}

			// Buat stiker
			stickerMsg := &proto.Message{
				StickerMessage: &proto.StickerMessage{
					Url:          waProto.String(url),
					DirectPath:   waProto.String(img.GetDirectPath()),
					MediaKey:     img.GetMediaKey(),
					Mimetype:     waProto.String("image/webp"),
					FileLength:   waProto.Uint64(img.GetFileLength()),
					PngThumbnail: imgData,
				},
			}
			_, err = client.SendMessage(context.Background(), from, stickerMsg)
			if err != nil {
				fmt.Printf("Error sending sticker: %v\n", err)
				_, _ = client.SendMessage(context.Background(), from, &proto.Message{
					Conversation: waProto.String("❌ Gagal bikin stiker, coba lagi!"),
				})
			}
			delete(userState, sender)
		}
	}
}
