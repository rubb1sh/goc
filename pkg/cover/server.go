/*
 Copyright 2020 Qiniu Cloud (qiniu.com)

 Licensed under the Apache License, Version 2.0 (the "License");
 you may not use this file except in compliance with the License.
 You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

package cover

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"golang.org/x/tools/cover"
	"k8s.io/test-infra/gopherage/pkg/cov"
)

// DefaultStore implements the IPersistence interface
var DefaultStore Store

// LogFile a file to save log.
const LogFile = "goc.log"

func init() {
	DefaultStore = NewFileStore()
}

// Run starts coverage host center
func Run(port string) {
	f, err := os.Create(LogFile)
	if err != nil {
		log.Fatalf("failed to create log file %s, err: %v", LogFile, err)
	}

	// both log to stdout and file by default
	mw := io.MultiWriter(f, os.Stdout)
	r := GocServer(mw)
	log.Fatal(r.Run(port))
}

// GocServer init goc server engine
func GocServer(w io.Writer) *gin.Engine {
	if w != nil {
		gin.DefaultWriter = w
	}
	r := gin.Default()
	// api to show the registered services
	r.StaticFile(PersistenceFile, "./"+PersistenceFile)

	v1 := r.Group("/v1")
	{
		v1.POST("/cover/register", registerService)
		v1.GET("/cover/profile", profile)
		v1.POST("/cover/clear", clear)
		v1.POST("/cover/init", initSystem)
		v1.GET("/cover/list", listServices)
	}

	return r
}

// Service is a entry under being tested
type Service struct {
	Name    string `form:"name" json:"name" binding:"required"`
	Address string `form:"address" json:"address" binding:"required"`
}

// ProfileParam is param of profile API (TODO)
type ProfileParam struct {
	Force   bool     `form:"force"`
	Service []string `form:"service" json:"service"`
	Address []string `form:"address" json:"address"`
}

//listServices list all the registered services
func listServices(c *gin.Context) {
	services := DefaultStore.GetAll()
	c.JSON(http.StatusOK, services)
}

func registerService(c *gin.Context) {
	var service Service
	if err := c.ShouldBind(&service); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	u, err := url.Parse(service.Address)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	realIP := c.ClientIP()
	if host != realIP {
		log.Printf("the registered host %s of service %s is different with the real one %s, here we choose the real one", service.Name, host, realIP)
		service.Address = fmt.Sprintf("http://%s:%s", realIP, port)
	}

	address := DefaultStore.Get(service.Name)
	if !contains(address, service.Address) {
		if err := DefaultStore.Add(service); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"result": "success"})
	return
}

func profile(c *gin.Context) {
	force, err := strconv.ParseBool(c.Query("force"))
	if err != nil {
		c.JSON(http.StatusExpectationFailed, gin.H{"error": "invalid param"})
		return
	}
	serviceList := removeDuplicateElement(c.QueryArray("service"))
	addressList := removeDuplicateElement(c.QueryArray("address"))
	allInfos := DefaultStore.GetAll()
	filterAddrList, err := filterAddrs(serviceList, addressList, force, allInfos)
	if err != nil {
		c.JSON(http.StatusExpectationFailed, gin.H{"error": err.Error()})
		return
	}

	var mergedProfiles = make([][]*cover.Profile, 0)
	for _, addr := range filterAddrList {
		pp, err := NewWorker(addr).Profile(ProfileParam{})
		if err != nil {
			if force {
				log.Warnf("get profile from [%s] failed, error: %s", addr, err.Error())
				continue
			}
			c.JSON(http.StatusExpectationFailed, gin.H{"error": err.Error()})
			return
		}
		profile, err := convertProfile(pp)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		mergedProfiles = append(mergedProfiles, profile)
	}

	if len(mergedProfiles) == 0 {
		c.JSON(http.StatusOK, "no profiles")
		return
	}

	merged, err := cov.MergeMultipleProfiles(mergedProfiles)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if err := cov.DumpProfile(merged, c.Writer); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
}

func clear(c *gin.Context) {
	svrsUnderTest := DefaultStore.GetAll()
	for svc, addrs := range svrsUnderTest {
		for _, addr := range addrs {
			pp, err := NewWorker(addr).Clear()
			if err != nil {
				c.JSON(http.StatusExpectationFailed, gin.H{"error": err.Error()})
				return
			}
			fmt.Fprintf(c.Writer, "Register service %s: %s coverage counter %s", svc, addr, string(pp))
		}
	}
}

func initSystem(c *gin.Context) {
	if err := DefaultStore.Init(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, "")
}

func convertProfile(p []byte) ([]*cover.Profile, error) {
	// Annoyingly, ParseProfiles only accepts a filename, so we have to write the bytes to disk
	// so it can read them back.
	// We could probably also just give it /dev/stdin, but that'll break on Windows.
	tf, err := ioutil.TempFile("", "")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file, err: %v", err)
	}
	defer tf.Close()
	defer os.Remove(tf.Name())
	if _, err := io.Copy(tf, bytes.NewReader(p)); err != nil {
		return nil, fmt.Errorf("failed to copy data to temp file, err: %v", err)
	}

	return cover.ParseProfiles(tf.Name())
}

func contains(arr []string, str string) bool {
	for _, element := range arr {
		if str == element {
			return true
		}
	}
	return false
}

// filterAddrs filter address list by given service and address list
func filterAddrs(serviceList, addressList []string, force bool, allInfos map[string][]string) (filterAddrList []string, err error) {
	addressAll := []string{}
	for _, addr := range allInfos {
		addressAll = append(addressAll, addr...)
	}
	if len(serviceList) != 0 && len(addressList) != 0 {
		return nil, fmt.Errorf("use 'service' flag and 'address' flag at the same time may cause ambiguity, please use them separately")
	}
	// Add matched services to map
	for _, name := range serviceList {
		if addr, ok := allInfos[name]; ok {
			filterAddrList = append(filterAddrList, addr...)
			continue // jump to match the next service
		}
		if !force {
			return nil, fmt.Errorf("service [%s] not found", name)
		}
		log.Warnf("service [%s] not found", name)
	}
	// Add matched addresses to map
	for _, addr := range addressList {
		if contains(addressAll, addr) {
			filterAddrList = append(filterAddrList, addr)
			continue
		}
		if !force {
			return nil, fmt.Errorf("address [%s] not found", addr)
		}
		log.Warnf("address [%s] not found", addr)
	}
	if len(addressList) == 0 && len(serviceList) == 0 {
		filterAddrList = addressAll
	}
	// Return all servers when all param is nil
	return filterAddrList, nil
}

// removeDuplicateElement remove duplicate element in slice
func removeDuplicateElement(addrs []string) []string {
	result := make([]string, 0, len(addrs))
	temp := map[string]struct{}{}
	for _, item := range addrs {
		if _, ok := temp[item]; !ok {
			temp[item] = struct{}{}
			result = append(result, item)
		}
	}
	return result
}
