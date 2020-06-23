package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	"github.com/google/go-cmp/cmp"
	gosros "github.com/karimra/go-sros"
	"github.com/openconfig/ygot/ygot"
	"github.com/openconfig/ygot/ytypes"
	"google.golang.org/protobuf/encoding/prototext"
)

func main() {
	if len(os.Args[1:]) != 2 {
		fmt.Println("needs exactly 2 files")
		return
	}
	file1 := os.Args[1]
	file2 := os.Args[2]
	b1, err := ioutil.ReadFile(file1)
	if err != nil {
		panic(err)
	}
	b2, err := ioutil.ReadFile(file2)
	if err != nil {
		panic(err)
	}

	d1 := new(gosros.Device)
	d2 := new(gosros.Device)
	err = gosros.Unmarshal(b1, d1)
	if err != nil {
		panic(err)
	}
	err = gosros.Unmarshal(b2, d2)
	if err != nil {
		panic(err)
	}

	if !cmp.Equal(d1, d2) {
		fmt.Println("- configurations differ")
	} else {
		fmt.Println("- configurations are similar")
		return
	}
	//validate
	err = d1.Validate(&ytypes.LeafrefOptions{IgnoreMissingData: true, Log: true})
	if err != nil {
		fmt.Printf("- config file '%s' is not valid\n", file1)
		for _, es := range strings.Split(err.Error(), ",") {
			for _, e := range strings.Split(es, ":") {
				fmt.Println(e)
			}
		}
	}
	err = d2.Validate(&ytypes.LeafrefOptions{IgnoreMissingData: true, Log: true})
	if err != nil {
		fmt.Printf("- config file '%s' is not valid\n", file2)
		for _, es := range strings.Split(err.Error(), ",") {
			for _, e := range strings.Split(es, ":") {
				fmt.Println(e)
			}
		}
	}

	gnmiNotif, err := ygot.Diff(d1, d2)
	if err != nil {
		panic(err)
	}
	fmt.Println("- gnmi notification with config delta:")
	fmt.Println(prototext.Format(gnmiNotif))
}
