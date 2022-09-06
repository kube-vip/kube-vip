package main

import (
	"github.com/kube-vip/kube-vip/cmd"
)

// Version is populated from the Makefile and is tied to the release TAG
var Version string

// Build is the last GIT commit
var Build string

func main() {

	// i, err := vip.CreateIptablesClient()
	// if err != nil {
	// 	panic(err)
	// }
	// if os.Getenv("CREATE") != "" {

	// 	b, err := i.CheckMangleChain(vip.MangleChainName)
	// 	if err != nil {
	// 		panic(err)
	// 	}
	// 	if b == false {
	// 		err = i.CreateMangleChain(vip.MangleChainName)
	// 		if err != nil {
	// 			panic(err)
	// 		}
	// 	}
	// 	// Add entries to our mangle chaing
	// 	err = i.AppendReturnRulesForDestinationSubnet(vip.MangleChainName, "10.0.0.0/16")
	// 	if err != nil {
	// 		panic(err)
	// 	}
	// 	err = i.AppendReturnRulesForDestinationSubnet(vip.MangleChainName, "10.96.0.0/12")
	// 	if err != nil {
	// 		panic(err)
	// 	}
	// 	err = i.AppendReturnRulesForMarking(vip.MangleChainName, "10.0.77.36/32")
	// 	if err != nil {
	// 		panic(err)
	// 	}
	// 	err = i.InsertMangeTableIntoPrerouting(vip.MangleChainName)
	// 	if err != nil {
	// 		panic(err)
	// 	}
	// 	err = i.InsertSourceNat("192.168.0.221", "10.0.77.36")
	// 	if err != nil {
	// 		panic(err)
	// 	}

	// 	err = i.DumpChain(vip.MangleChainName)
	// 	vip.ExampleNfct_Dump()
	// 	if err == nil {
	// 		return
	// 	}
	// }
	// if os.Getenv("DELETE") != "" {
	// 	err = i.DeleteManglePrerouting(vip.MangleChainName)
	// 	if err != nil {
	// 		panic(err)
	// 	}
	// 	err = i.DeleteMangleChain(vip.MangleChainName)
	// 	if err != nil {
	// 		panic(err)
	// 	}
	// 	err = i.DeleteSourceNat("10.0.77.36", "192.168.0.221")
	// 	if err != nil {
	// 		panic(err)
	// 	}
	// 	//err = i.DumpChain(vip.MangleChainName)
	// 	vip.ExampleNfct_Dump()
	// 	if err == nil {
	// 		return
	// 	}
	// 	if err != nil {
	// 		panic(err)
	// 	}
	// }
	cmd.Release.Version = Version
	cmd.Release.Build = Build
	cmd.Execute()
}
