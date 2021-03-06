package agent

import (
	"runtime"
	"strings"
	"testing"

	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/coreos/go-etcd/etcd"
	"github.com/flynn/flynn/discoverd/testutil/etcdrunner"
)

func runEtcdServer(t *testing.T) (*etcd.Client, func()) {
	addr, kill := etcdrunner.RunEtcdServer(t)
	return etcd.NewClient([]string{addr}), kill
}

const NoAttrService = "null"

func TestEtcdBackend_RegisterAndUnregister(t *testing.T) {
	client, done := runEtcdServer(t)
	defer done()

	backend := EtcdBackend{Client: client}
	serviceName := "test_register"
	serviceAddr := "127.0.0.1"

	client.Delete(KeyPrefix+"/services/"+serviceName+"/"+serviceAddr, true)
	backend.Register(serviceName, serviceAddr, nil)

	servicePath := KeyPrefix + "/services/" + serviceName + "/" + serviceAddr
	response, err := client.Get(servicePath, false, false)
	if err != nil {
		t.Fatal(err)
	}

	// Check if the files the returned values are the same.
	if (response.Node.Key != servicePath) || (response.Node.Value != NoAttrService) {
		t.Fatal("Returned value not equal to sent one")
	}

	backend.Unregister(serviceName, serviceAddr)
	_, err = client.Get(servicePath, false, false)
	if err == nil {
		t.Fatal("Value not deleted after unregister")
	}
}

func TestEtcdBackend_Attributes(t *testing.T) {
	client, done := runEtcdServer(t)
	defer done()

	backend := EtcdBackend{Client: client}
	serviceName := "test_attributes"
	serviceAddr := "127.0.0.1"
	serviceAttrs := map[string]string{
		"foo": "bar",
		"baz": "qux",
	}

	client.Delete(KeyPrefix+"/services/"+serviceName+"/"+serviceAddr, true)
	backend.Register(serviceName, serviceAddr, serviceAttrs)
	defer backend.Unregister(serviceName, serviceAddr)

	updates, _ := backend.Subscribe(serviceName)
	runtime.Gosched()

	update := <-updates.Chan()
	if update.Attrs["foo"] != "bar" || update.Attrs["baz"] != "qux" {
		t.Fatal("Attributes received are not attributes registered")
	}
}

func TestEtcdBackend_Subscribe(t *testing.T) {
	client, done := runEtcdServer(t)
	defer done()

	backend := EtcdBackend{Client: client}

	err := backend.Register("test_subscribe", "10.0.0.1", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Unregister("test_subscribe", "10.0.0.1")

	backend.Register("test_subscribe", "10.0.0.2", nil)
	defer backend.Unregister("test_subscribe", "10.0.0.2")

	updates, _ := backend.Subscribe("test_subscribe")
	runtime.Gosched()

	backend.Register("test_subscribe", "10.0.0.3", nil)
	defer backend.Unregister("test_subscribe", "10.0.0.3")

	backend.Register("test_subscribe", "10.0.0.4", nil)
	defer backend.Unregister("test_subscribe", "10.0.0.4")

	for i := 0; i < 5; i++ {
		update := <-updates.Chan()
		if update.Addr == "" && update.Name == "" {
			continue // skip the update that signals "up to current" event
		}
		if update.Online != true {
			t.Fatal("Unexpected offline service update: ", update, i)
		}
		if !strings.Contains("10.0.0.1 10.0.0.2 10.0.0.3 10.0.0.4", update.Addr) {
			t.Fatal("Service update of unexected addr: ", update, i)
		}
	}

	backend.Register("test_subscribe", "10.0.0.5", nil)
	backend.Unregister("test_subscribe", "10.0.0.5")

	<-updates.Chan()           // .5 comes online
	update := <-updates.Chan() // .5 goes offline
	if update.Addr != "10.0.0.5" {
		t.Fatal("Unexpected addr: ", update)
	}
	if update.Online != false {
		t.Fatal("Expected service to be offline:", update)
	}
}
