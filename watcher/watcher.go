package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/google/gnxi/utils/xpath"
	gosros "github.com/karimra/go-sros"
	"github.com/openconfig/gnmi/proto/gnmi"
	"github.com/openconfig/ygot/ygot"
	"github.com/openconfig/ygot/ytypes"
	"github.com/spf13/pflag"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

type target struct {
	address  string
	username string
	password string
	insecure bool
	path     []string
	encoding string
}

func main() {
	tg := new(target)
	var dconf string
	pflag.StringVarP(&tg.address, "address", "a", "", "target address")
	pflag.StringVarP(&tg.username, "username", "u", "", "target username")
	pflag.StringVarP(&tg.password, "password", "p", "", "target password")
	pflag.BoolVarP(&tg.insecure, "insecure", "i", false, "insecure gRPC connection")
	pflag.StringSliceVarP(&tg.path, "path", "", []string{""}, "path to subscribe to. (on_change)")
	pflag.StringVarP(&tg.encoding, "encoding", "e", "json", "subscription encoding")
	pflag.StringVarP(&dconf, "desired-config", "c", "", "yaml/json file with target desired config")
	pflag.Parse()

	desiredConfig, err := ioutil.ReadFile(dconf)
	if err != nil {
		panic(err)
	}
	if filepath.Ext(dconf) == ".yaml" || filepath.Ext(dconf) == ".yml" {
		jsonConf := new(interface{})
		err = json.Unmarshal(desiredConfig, jsonConf)
		if err != nil {
			panic(err)
		}
		desiredConfig, err = json.Marshal(jsonConf)
		if err != nil {
			panic(err)
		}
	}
	desiredDeviceConfig := new(gosros.Device)
	err = gosros.Unmarshal(desiredConfig, desiredDeviceConfig)
	if err != nil {
		panic(err)
	}
	//
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	subReq, err := createSubReq(tg)
	if err != nil {
		panic(err)
	}
	nctx, cancel := context.WithCancel(ctx)
	defer cancel()
	nctx = metadata.AppendToOutgoingContext(nctx, "username", tg.username, "password", tg.password)
	conn, err := createGrpcConn(ctx, tg)
	if err != nil {
		panic(err)
	}
	client := gnmi.NewGNMIClient(conn)
	notifChan := make(chan *gnmi.SubscribeResponse_Update, 0)
	go watcher(nctx, client, desiredDeviceConfig, notifChan)

	subscribeClient, err := client.Subscribe(nctx)
	err = subscribeClient.Send(subReq)
	if err != nil {
		log.Printf("subscribe error: %v", err)
		return
	}
	for {
		select {
		case <-ctx.Done():
			log.Printf("context done")
			return
		default:
			subscribeRsp, err := subscribeClient.Recv()
			if err != nil {
				log.Printf("addr=%s rcv error: %v", tg.address, err)
				return
			}
			switch resp := subscribeRsp.Response.(type) {
			case *gnmi.SubscribeResponse_Update:
				notifChan <- resp
			case *gnmi.SubscribeResponse_SyncResponse:
				log.Printf("received sync response=%+v from %s\n", resp.SyncResponse, tg.address)
			}
		}
	}
	//
}

func createSubReq(tg *target) (*gnmi.SubscribeRequest, error) {
	subscriptions := make([]*gnmi.Subscription, len(tg.path))
	for i, p := range tg.path {
		gnmiPath, err := xpath.ToGNMIPath(strings.TrimSpace(p))
		if err != nil {
			return nil, fmt.Errorf("path parse error: %v", err)
		}
		subscriptions[i] = &gnmi.Subscription{Path: gnmiPath}
		subscriptions[i].Mode = gnmi.SubscriptionMode_ON_CHANGE
		//subscriptions[i].HeartbeatInterval = uint64(time.Minute.Nanoseconds())
	}

	encodingVal, ok := gnmi.Encoding_value[strings.Replace(strings.ToUpper(tg.encoding), "-", "_", -1)]
	if !ok {
		return nil, fmt.Errorf("invalid encoding type '%s'", tg.encoding)
	}

	subReq := &gnmi.SubscribeRequest{
		Request: &gnmi.SubscribeRequest_Subscribe{
			Subscribe: &gnmi.SubscriptionList{
				Mode:         gnmi.SubscriptionList_STREAM,
				Encoding:     gnmi.Encoding(encodingVal),
				Subscription: subscriptions,
				// Qos:          qos,
				// UpdatesOnly:  viper.GetBool("updates-only"),
			},
		},
	}
	return subReq, nil
}
func createGrpcConn(ctx context.Context, tg *target) (*grpc.ClientConn, error) {
	opts := []grpc.DialOption{}
	opts = append(opts, grpc.WithBlock())
	if tg.insecure {
		opts = append(opts, grpc.WithInsecure())
	} else {
		tlsConfig := &tls.Config{
			Renegotiation:      tls.RenegotiateNever,
			InsecureSkipVerify: true,
		}
		// err := loadCerts(tlsConfig)
		// if err != nil {
		// 	logger.Printf("failed loading certificates: %v", err)
		// }

		// err = loadCACerts(tlsConfig)
		// if err != nil {
		// 	logger.Printf("failed loading CA certificates: %v", err)
		// }
		opts = append(opts, grpc.WithDisableRetry())
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(timeoutCtx, tg.address, opts...)
	if err != nil {
		return nil, err
	}
	return conn, nil
}
func watcher(ctx context.Context, client gnmi.GNMIClient, desiredConf *gosros.Device, c chan *gnmi.SubscribeResponse_Update) {
	rootSchema := mustSchema(gosros.Schema).RootSchema()
	for {
	out:
		select {
		case upd := <-c:
			log.Printf("got update: %v", upd)
			newDevice, err := ygot.DeepCopy(desiredConf)
			if err != nil {
				log.Printf("failed deepCopy: %v", err)
				continue
			}
			for _, u := range upd.Update.GetUpdate() {
				fullPath := upd.Update.Prefix
				fullPath.Elem = append(fullPath.Elem, u.Path.Elem...)
				err = ytypes.SetNode(rootSchema, newDevice, fullPath, u.Value,
					&ytypes.InitMissingElements{},
					&ytypes.TolerateJSONInconsistencies{},
				)
				if err != nil {
					log.Printf("FAILED SETTING NODE: %v", err)
					break out
				}

				nodes, err := ytypes.GetNode(rootSchema, newDevice, fullPath)
				if err != nil {
					log.Printf("FAILED GETTING NODE: %v", err)
				}
				for _, n := range nodes {
					fmt.Println(n.Path)
					spew.Dump(n.Data)
				}

			}

			gnmiNotifs, err := ygot.Diff(newDevice, desiredConf)
			if err != nil {
				log.Printf("failed getting the diff of desired and new config")
				continue
			}
			log.Printf("resulting gnmiNotifications: %v", gnmiNotifs)
			for i := range gnmiNotifs.Update {
				gnmiNotifs.Update[i].Val.Value, err = toJSONValue(gnmiNotifs.Update[i].Val)
				if err != nil {
					log.Printf("failed converting gnmi TypedValue: %v", err)
					continue
				}
			}
			if len(gnmiNotifs.GetUpdate()) == 0 && len(gnmiNotifs.GetDelete()) == 0 {
				log.Printf("no notifs")
				continue
			}
			req := &gnmi.SetRequest{
				Delete: gnmiNotifs.GetDelete(),
				Update: gnmiNotifs.GetUpdate(),
			}

			log.Printf("sending set request: %v", req)
			resp, err := client.Set(ctx, req)
			if err != nil {
				log.Printf("failed sending gnmi Set to target: %v, %v", req, err)
				continue
			}
			log.Printf("got set response: %+v", resp)
		case <-ctx.Done():
			return
		}
	}
}

func mustSchema(fn func() (*ytypes.Schema, error)) *ytypes.Schema {
	s, err := fn()
	if err != nil {
		panic(err)
	}
	return s
}

func toJSONValue(updValue *gnmi.TypedValue) (*gnmi.TypedValue_JsonVal, error) {
	if updValue == nil {
		return nil, nil
	}
	value := new(gnmi.TypedValue_JsonVal)
	switch updValue.Value.(type) {
	case *gnmi.TypedValue_AsciiVal:
		b, err := json.Marshal(updValue.GetAsciiVal())
		if err != nil {
			return nil, err
		}
		value = &gnmi.TypedValue_JsonVal{
			JsonVal: b,
		}
	case *gnmi.TypedValue_BoolVal:
		b, err := json.Marshal(updValue.GetBoolVal())
		if err != nil {
			return nil, err
		}
		value = &gnmi.TypedValue_JsonVal{
			JsonVal: b,
		}
	case *gnmi.TypedValue_BytesVal:
		b, err := json.Marshal(updValue.GetBytesVal())
		if err != nil {
			return nil, err
		}
		value = &gnmi.TypedValue_JsonVal{
			JsonVal: b,
		}
	case *gnmi.TypedValue_DecimalVal:
		b, err := json.Marshal(updValue.GetDecimalVal())
		if err != nil {
			return nil, err
		}
		value = &gnmi.TypedValue_JsonVal{
			JsonVal: b,
		}
	case *gnmi.TypedValue_FloatVal:
		b, err := json.Marshal(updValue.GetFloatVal())
		if err != nil {
			return nil, err
		}
		value = &gnmi.TypedValue_JsonVal{
			JsonVal: b,
		}
	case *gnmi.TypedValue_IntVal:
		b, err := json.Marshal(updValue.GetIntVal())
		if err != nil {
			return nil, err
		}
		value = &gnmi.TypedValue_JsonVal{
			JsonVal: b,
		}
	case *gnmi.TypedValue_StringVal:
		b, err := json.Marshal(updValue.GetStringVal())
		if err != nil {
			return nil, err
		}
		value = &gnmi.TypedValue_JsonVal{
			JsonVal: b,
		}
	case *gnmi.TypedValue_UintVal:
		b, err := json.Marshal(updValue.GetUintVal())
		if err != nil {
			return nil, err
		}
		value = &gnmi.TypedValue_JsonVal{
			JsonVal: b,
		}
	case *gnmi.TypedValue_JsonIetfVal:
		value = &gnmi.TypedValue_JsonVal{
			JsonVal: updValue.GetJsonIetfVal(),
		}
	case *gnmi.TypedValue_JsonVal:
		value = &gnmi.TypedValue_JsonVal{
			JsonVal: updValue.GetJsonVal(),
		}
	case *gnmi.TypedValue_LeaflistVal:
		b, err := json.Marshal(updValue.GetLeaflistVal())
		if err != nil {
			return nil, err
		}
		value = &gnmi.TypedValue_JsonVal{
			JsonVal: b,
		}
	case *gnmi.TypedValue_ProtoBytes:
		b, err := json.Marshal(updValue.GetProtoBytes())
		if err != nil {
			return nil, err
		}
		value = &gnmi.TypedValue_JsonVal{
			JsonVal: b,
		}
	case *gnmi.TypedValue_AnyVal:
		b, err := json.Marshal(updValue.GetAnyVal())
		if err != nil {
			return nil, err
		}
		value = &gnmi.TypedValue_JsonVal{
			JsonVal: b,
		}
	}
	return value, nil
}
