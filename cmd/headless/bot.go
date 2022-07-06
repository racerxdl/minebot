package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-gl/mathgl/mgl32"
	"github.com/racerxdl/minebot/config"
	"github.com/racerxdl/minebot/lang"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/auth"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"github.com/sandertv/gophertunnel/minecraft/text"
	"github.com/sirupsen/logrus"
)

var log = logrus.New()

type Player struct {
	Username        string
	EntityRuntimeID uint64
	EntityGlobalID  int64
	Position        mgl32.Vec3
}

var players = map[uint64]*Player{}

func handlePacket(conn *minecraft.Conn, pk packet.Packet) {
	if pk != nil {
		switch pk.ID() {
		case packet.IDText:
			txt := pk.(*packet.Text)
			if txt.TextType != packet.TextTypeObjectWhisper {
				if txt.NeedsTranslation {
					txt.Message = lang.FormatString("ptbr", txt.Message)
					anyParameters := make([]any, len(txt.Parameters))
					for i, v := range txt.Parameters {
						anyParameters[i] = lang.GetString("ptbr", v)
					}
					txt.Message = fmt.Sprintf(txt.Message, anyParameters...)
				}
				msg := text.ANSI(txt.Message)
				log.Infof("%s> %s\n", txt.SourceName, msg)
			}

		case packet.IDPlayerList:
			list := pk.(*packet.PlayerList)
			log.Infof("Received player list with %d players\n", len(list.Entries))
			for _, v := range list.Entries {
				log.Infof("User: %s EntityID: %d\n", v.Username, v.EntityUniqueID)
			}

		case packet.IDRemoveEntity:
			rement := pk.(*packet.RemoveEntity)
			player, ok := players[rement.EntityNetworkID]
			if ok {
				delete(players, rement.EntityNetworkID)
				log.Infof("Player %s went of range\n", player.Username)
			}

		case packet.IDAddPlayer:
			addent := pk.(*packet.AddPlayer)
			log.Infof("Player %s added to %s\n", addent.Username, addent.Position)
			players[addent.EntityRuntimeID] = &Player{
				Username:        addent.Username,
				EntityRuntimeID: addent.EntityRuntimeID,
				EntityGlobalID:  addent.EntityUniqueID,
				Position:        addent.Position,
			}

		case packet.IDActorEvent:
			event := pk.(*packet.ActorEvent)
			if event.EventType == packet.EventTypePlayerDied {
				log.Debugf("Entity %d died\n", event.EntityRuntimeID)
				player, ok := players[event.EntityRuntimeID]
				if ok {
					log.Warnf("Player %s died\n", player.Username)
				}
			}

		case packet.IDMovePlayer:
			mv := pk.(*packet.MovePlayer)
			player, ok := players[mv.EntityRuntimeID]
			if ok {
				if !player.Position.ApproxEqual(mv.Position) {
					player.Position = mv.Position
					//log.Infof("Player %s at %s\n", player.Username, player.Position)
				}
			}

		case packet.IDMoveActorAbsolute:
			mv := pk.(*packet.MoveActorAbsolute)
			player, ok := players[mv.EntityRuntimeID]
			if ok {
				if !player.Position.ApproxEqual(mv.Position) {
					player.Position = mv.Position
					//log.Infof("Player %s at %s\n", player.Username, player.Position)
				}
			}

		case packet.IDMoveActorDelta:
			mv := pk.(*packet.MoveActorDelta)
			player, ok := players[mv.EntityRuntimeID]
			if ok {
				//old := player.Position
				player.Position = player.Position.Add(mv.Position)
				//if !player.Position.ApproxEqual(old) {
				//	log.Infof("Player %s at %s\n", player.Username, player.Position)
				//}
			}
		}
	}
}

var loopRunning = false

func eventTxLoop(conn *minecraft.Conn, wg *sync.WaitGroup, stop chan struct{}) {
	defer wg.Done()
	loopRunning = true
	t := time.NewTicker(time.Second * 5)
	defer t.Stop()
	log.Info("TX Event loop started\n")
	for loopRunning {
		time.Sleep(time.Millisecond * 100)

		select {
		case <-stop:
			log.Infof("closing event loop\n")
			loopRunning = false
			_ = conn.Close()
			//case <-t.C:
			//	log.Infof("Sending message\n")
			//	txt := &packet.Text{
			//		Message:          text.Colourf("<B>The time now is</B>: <red>%s</red>", time.Now().Format(time.RFC822Z)),
			//		SourceName:       "BOT",
			//		NeedsTranslation: false,
			//		TextType:         packet.TextTypeRaw,
			//	}
			//
			//	if err := conn.WritePacket(txt); err != nil {
			//		log.Errorf("Error sending message: %s\n", err)
			//	}
		}
	}
}

func eventRxLoop(conn *minecraft.Conn, wg *sync.WaitGroup) {
	defer wg.Done()
	log.Info("RX Event loop started\n")
	for {
		pk, err := conn.ReadPacket()
		if err != nil {
			if disconnect, ok := errors.Unwrap(err).(minecraft.DisconnectError); ok {
				log.Errorf("Disconnected: %s\n", disconnect.Error())
				panic(disconnect)
			}
			loopRunning = false
			return
		}
		handlePacket(conn, pk)
	}
}

func main() {
	log.Info("Loading configuration\n")
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("error loading config: %s\n", err)
	}

	log.Info("Loading Xbox Token\n")
	tkn, err := config.LoadToken()
	if err != nil {
		tkn, err = auth.RequestLiveToken()
		if err != nil {
			log.Fatalf("error getting token: %s\n", err)
		}
		err = config.SaveToken(tkn)
		if err != nil {
			log.Fatalf("error saving token: %s\n", err)
		}
	}
	src := auth.RefreshTokenSource(tkn)
	var conn *minecraft.Conn

	log.Infof("Connecting to %s\n", cfg.Connection.RemoteAddress)
	connected := false

	for !connected {
		conn, err = minecraft.Dialer{
			TokenSource: src,
		}.Dial("raknet", cfg.Connection.RemoteAddress)
		if err != nil {
			if disconnect, ok := errors.Unwrap(err).(minecraft.DisconnectError); ok {
				v := lang.GetString("ptbr", disconnect.Error())
				log.Errorf("Disconnected: %s\n", v)
			} else {
				log.Errorf("Error handling connection: %s\n", err)
			}
			time.Sleep(time.Second)
		} else {
			connected = true
		}
	}
	defer func() {
		_ = conn.Close()
	}()

	c := make(chan os.Signal)
	stop := make(chan struct{}, 1)
	wg := &sync.WaitGroup{}
	wg.Add(2)

	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		log.Info("Closing bot\n")

		t := time.NewTimer(time.Second * 5)
		defer t.Stop()
		select {
		case stop <- struct{}{}:
		case <-t.C:
			log.Errorln("timeout waiting close")
		}
		_ = conn.Close()
		log.Infoln("Gotcha. KTHXBYE")
	}()

	log.Info("Bot started and connected\n")
	go eventRxLoop(conn, wg)
	go eventTxLoop(conn, wg, stop)

	wg.Wait()
}
