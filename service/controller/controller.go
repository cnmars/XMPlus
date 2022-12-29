package controller

import (
	"fmt"
	"log"
	"reflect"
	"time"
	"strings"

	"github.com/xcode75/xcore/common/protocol"
	"github.com/xcode75/xcore/common/serial"
	"github.com/xcode75/xcore/common/task"
	"github.com/xcode75/xcore/core"
	"github.com/xcode75/xcore/features"
	"github.com/xcode75/xcore/features/inbound"
	"github.com/xcode75/xcore/features/outbound"
	"github.com/xcode75/xcore/features/routing"
	"github.com/xcode75/xcore/features/stats"
	"github.com/xcode75/xcore/infra/conf"
	"github.com/xcode75/xcore/app/router"
	
	C "github.com/sagernet/sing/common"
	"github.com/sagernet/sing-shadowsocks/shadowaead_2022"

	"github.com/xcode75/XMPlus/api"
	"github.com/xcode75/XMPlus/app/mydispatcher"
	"github.com/xcode75/XMPlus/common/mylego"
	"github.com/xcode75/XMPlus/common/serverstatus"
)

type Controller struct {
	server       *core.Instance
	config       *Config
	clientInfo   api.ClientInfo
	apiClient    api.API
	nodeInfo     *api.NodeInfo
	relaynodeInfo *api.RelayNodeInfo
	Tag          string
	RelayTag     string
	Relay        bool
	userList     *[]api.UserInfo
	tasks        []periodicTask
	ibm          inbound.Manager
	obm          outbound.Manager
	stm          stats.Manager
	dispatcher   *mydispatcher.DefaultDispatcher
	rdispatcher  *router.Router
	startAt      time.Time
}

type periodicTask struct {
	tag string
	*task.Periodic
}

// New return a Controller service with default parameters.
func New(server *core.Instance, api api.API, config *Config) *Controller {
	controller := &Controller{
		server:     server,
		config:     config,
		apiClient:  api,
		ibm:        server.GetFeature(inbound.ManagerType()).(inbound.Manager),
		obm:        server.GetFeature(outbound.ManagerType()).(outbound.Manager),
		stm:        server.GetFeature(stats.ManagerType()).(stats.Manager),
		dispatcher: server.GetFeature(routing.DispatcherType()).(*mydispatcher.DefaultDispatcher),
		rdispatcher: server.GetFeature(routing.RouterType()).(*router.Router),
		startAt:    time.Now(),
	}

	return controller
}

// Start implement the Start() function of the service interface
func (c *Controller) Start() error {
	c.clientInfo = c.apiClient.Describe()
	// First fetch Node Info
	newNodeInfo, err := c.apiClient.GetNodeInfo()
	if err != nil {
		return err
	}
	c.nodeInfo = newNodeInfo
	c.Tag = c.buildNodeTag()

	// append remote DNS config and init dns service
	err = c.addNewDNS(newNodeInfo)
	if err != nil {
		return err
	}

	// Update user
	userInfo, err := c.apiClient.GetUserList()
	if err != nil {
		return err
	}

	// sync controller userList
	c.userList = userInfo
	
	c.Relay = false
	
	// Add new Relay	tag
	if c.nodeInfo.Relay {
		newRelayNodeInfo, err := c.apiClient.GetRelayNodeInfo()
		if err != nil {
			log.Panic(err)
			return nil
		}	
		c.relaynodeInfo = newRelayNodeInfo
		c.RelayTag = c.buildRNodeTag()
		
		log.Printf("%s Taking a Detour Route [%s] For Users", c.logPrefix(), c.RelayTag)
		err = c.addNewRelayTag(newRelayNodeInfo, userInfo)
		if err != nil {
			log.Panic(err)
			return err
		}
		c.Relay = true
	}
	
	// Add new tag
	err = c.addNewTag(newNodeInfo)
	if err != nil {
		log.Panic(err)
		return err
	}

	err = c.addNewUser(userInfo, newNodeInfo)
	if err != nil {
		return err
	}

	// Add Limiter
	if err := c.AddInboundLimiter(c.Tag, newNodeInfo.SpeedLimit, userInfo); err != nil {
		log.Print(err)
	}

	// Add Rule Manager

	if ruleList, err := c.apiClient.GetNodeRule(); err != nil {
		log.Printf("Get rule list filed: %s", err)
	} else if len(*ruleList) > 0 {
		if err := c.UpdateRule(c.Tag, *ruleList); err != nil {
			log.Print(err)
		}
	}

	// Add periodic tasks
	c.tasks = append(c.tasks,
		periodicTask{
			tag: "node monitor",
			Periodic: &task.Periodic{
				Interval: time.Duration(c.config.UpdatePeriodic) * time.Second,
				Execute:  c.nodeInfoMonitor,
			}},
		periodicTask{
			tag: "user monitor",
			Periodic: &task.Periodic{
				Interval: time.Duration(c.config.UpdatePeriodic) * time.Second,
				Execute:  c.userInfoMonitor,
			}},
	)

	// Check cert service in need
	if c.nodeInfo.EnableTLS {
		c.tasks = append(c.tasks, periodicTask{
			tag: "cert monitor",
			Periodic: &task.Periodic{
				Interval: time.Duration(c.config.UpdatePeriodic) * time.Second * 60,
				Execute:  c.certMonitor,
			}})
	}

	// Start periodic tasks
	for i := range c.tasks {
		log.Printf("%s Task Scheduler for %s started", c.logPrefix(), c.tasks[i].tag)
		go c.tasks[i].Start()
	}

	return nil
}

// Close implement the Close() function of the service interface
func (c *Controller) Close() error {
	for i := range c.tasks {
		if c.tasks[i].Periodic != nil {
			if err := c.tasks[i].Periodic.Close(); err != nil {
				log.Panicf("%s Task Scheduler for  %s failed to close: %s", c.logPrefix(), c.tasks[i].tag, err)
			}
		}
	}

	return nil
}

func (c *Controller) nodeInfoMonitor() (err error) {
	// delay to start
	if time.Since(c.startAt) < time.Duration(c.config.UpdatePeriodic)*time.Second {
		return nil
	}

	// First fetch Node Info
	newNodeInfo, err := c.apiClient.GetNodeInfo()
	if err != nil {
		log.Print(err)
		return nil
	}

	// Update User
	var usersChanged = true
	newUserInfo, err := c.apiClient.GetUserList()
	if err != nil {
		if err.Error() == "users no change" {
			usersChanged = false
			newUserInfo = c.userList
		} else {
			log.Print(err)
			return nil
		}
	}
	
	var updateRelay = false
	// Remove user relay rule
	if usersChanged {
		updateRelay = true
		c.removeRules(c.Tag, c.userList)
	}
	
	var nodeInfoChanged = false
	// If nodeInfo changed
	if !reflect.DeepEqual(c.nodeInfo, newNodeInfo) {
		// Remove old tag
		oldTag := c.Tag
		err := c.removeOldTag(oldTag)
		if err != nil {
			log.Print(err)
			return nil
		}
		if c.nodeInfo.NodeType == "Shadowsocks-Plugin" {
			err = c.removeOldTag(fmt.Sprintf("dokodemo-door_%s+1", c.Tag))
		}
		if err != nil {
			log.Print(err)
			return nil
		}
		updateRelay = true
		// Add new tag
		c.nodeInfo = newNodeInfo
		c.Tag = c.buildNodeTag()
		err = c.addNewTag(newNodeInfo)
		if err != nil {
			log.Print(err)
			return nil
		}
		nodeInfoChanged = true
		// Remove Old limiter
		if err = c.DeleteInboundLimiter(oldTag); err != nil {
			log.Print(err)
			return nil
		}
	}
	
	// Remove relay tag
	if c.Relay && updateRelay {
		err := c.removeRelayTag(c.RelayTag, c.userList)
		if err != nil {
			return err
		}
		c.Relay = false
	}
	
	// Update new Relay tag
	if c.nodeInfo.Relay && updateRelay {
		newRelayNodeInfo, err := c.apiClient.GetRelayNodeInfo()
		if err != nil {
			log.Panic(err)
			return nil
		}	
		c.relaynodeInfo = newRelayNodeInfo
		c.RelayTag = c.buildRNodeTag()
		
		log.Printf("%s Reload Detour Route [%s] For Users", c.logPrefix(), c.RelayTag)
		
		err = c.addNewRelayTag(newRelayNodeInfo, newUserInfo)
		if err != nil {
			log.Panic(err)
			return err
		}
		c.Relay = true
	}	
	
	// Check Rule

	if ruleList, err := c.apiClient.GetNodeRule(); err != nil {
		log.Printf("Get rule list filed: %s", err)
	} else if len(*ruleList) > 0 {
		if err := c.UpdateRule(c.Tag, *ruleList); err != nil {
			log.Print(err)
		}
	}
	

	if nodeInfoChanged {
		err = c.addNewUser(newUserInfo, newNodeInfo)
		if err != nil {
			log.Print(err)
			return nil
		}

		// Add Limiter
		if err := c.AddInboundLimiter(c.Tag, newNodeInfo.SpeedLimit, newUserInfo); err != nil {
			log.Print(err)
			return nil
		}

		// Add DNS
		log.Printf("%s Reload DNS service", c.logPrefix())
		if err := c.addNewDNS(newNodeInfo); err != nil {
			log.Print(err)
			return nil
		}
	} else {
		var deleted, added []api.UserInfo
		if usersChanged {
			deleted, added = compareUserList(c.userList, newUserInfo)
			if len(deleted) > 0 {
				deletedEmail := make([]string, len(deleted))
				for i, u := range deleted {
					deletedEmail[i] = fmt.Sprintf("%s|%s|%d", c.Tag, u.Email, u.UID)
				}
				err := c.removeUsers(deletedEmail, c.Tag)
				if err != nil {
					log.Print(err)
				}
				log.Printf("%s %d Users Deleted", c.logPrefix(), len(deleted))
			}
			if len(added) > 0 {
				err = c.addNewUser(&added, c.nodeInfo)
				if err != nil {
					log.Print(err)
				}
				// Update Limiter
				if err := c.UpdateInboundLimiter(c.Tag, &added); err != nil {
					log.Print(err)
				}
				log.Printf("%s %d New Users Added", c.logPrefix(), len(added))
			}
		}
		
	}
	c.userList = newUserInfo
	return nil
}

func (c *Controller) removeOldTag(oldTag string) (err error) {
	err = c.removeInbound(oldTag)
	if err != nil {
		return err
	}
	err = c.removeOutbound(oldTag)
	if err != nil {
		return err
	}
	return nil
}

func (c *Controller) addNewTag(newNodeInfo *api.NodeInfo) (err error) {
	if newNodeInfo.NodeType != "Shadowsocks-Plugin" {
		inboundConfig, err := InboundBuilder(c.config, newNodeInfo, c.Tag)
		if err != nil {
			return err
		}
		err = c.addInbound(inboundConfig)
		if err != nil {

			return err
		}
		if !c.nodeInfo.Relay {
			outBoundConfig, err := OutboundBuilder(c.config, newNodeInfo, c.Tag)
			if err != nil {

				return err
			}
			err = c.addOutbound(outBoundConfig)
			if err != nil {

				return err
			}
		}

	} else {
		return c.addInboundForSSPlugin(*newNodeInfo)
	}
	return nil
}


func (c *Controller) removeRelayTag(tag string, userInfo *[]api.UserInfo) (err error) {
	for _, user := range *userInfo {
		err = c.removeOutbound(fmt.Sprintf("%s_%d", tag, user.UID))
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) removeRules(tag string, userInfo *[]api.UserInfo){
	for _, user := range *userInfo {
		c.RemoveUsersRule([]string{c.buildUserTag(&user)})			
	}	
}

func (c *Controller) addNewRelayTag(newRelayNodeInfo *api.RelayNodeInfo, userInfo *[]api.UserInfo) (err error) {
	if newRelayNodeInfo.NodeType != "Shadowsocks-Plugin" {
		for _, user := range *userInfo {
			var Key string			
			if C.Contains(shadowaead_2022.List, strings.ToLower(newRelayNodeInfo.CypherMethod)) {
				userKey, err := c.checkShadowsocksPassword(user.Passwd, newRelayNodeInfo.CypherMethod)
				if err != nil {
					newError(fmt.Errorf("[UID: %d] %s", user.UUID, err)).AtError().WriteToLog()
					continue
				}
				Key = fmt.Sprintf("%s:%s", newRelayNodeInfo.ServerKey, userKey)
			} else {
				Key = user.Passwd
			}
			RelayTagConfig, err := OutboundRelayBuilder(c.config, newRelayNodeInfo, c.RelayTag, user.UUID, user.Email, Key, user.UID)
			if err != nil {
				return err
			}
			
			err = c.addOutbound(RelayTagConfig)
			if err != nil {
				return err
			}
			c.AddUsersRule(fmt.Sprintf("%s_%d", c.RelayTag, user.UID), []string{c.buildUserTag(&user)})		
		}
	}
	return nil
}

func (c *Controller) addInboundForSSPlugin(newNodeInfo api.NodeInfo) (err error) {
	// Shadowsocks-Plugin require a separate inbound for other TransportProtocol likes: ws, grpc
	fakeNodeInfo := newNodeInfo
	fakeNodeInfo.TransportProtocol = "tcp"
	fakeNodeInfo.EnableTLS = false
	// Add a regular Shadowsocks inbound and outbound
	inboundConfig, err := InboundBuilder(c.config, &fakeNodeInfo, c.Tag)
	if err != nil {
		return err
	}
	err = c.addInbound(inboundConfig)
	if err != nil {

		return err
	}
	outBoundConfig, err := OutboundBuilder(c.config, &fakeNodeInfo, c.Tag)
	if err != nil {

		return err
	}
	err = c.addOutbound(outBoundConfig)
	if err != nil {

		return err
	}
	// Add an inbound for upper streaming protocol
	fakeNodeInfo = newNodeInfo
	fakeNodeInfo.Port++
	fakeNodeInfo.NodeType = "dokodemo-door"
	dokodemoTag := fmt.Sprintf("dokodemo-door_%s+1", c.Tag)
	inboundConfig, err = InboundBuilder(c.config, &fakeNodeInfo, dokodemoTag)
	if err != nil {
		return err
	}
	err = c.addInbound(inboundConfig)
	if err != nil {

		return err
	}
	outBoundConfig, err = OutboundBuilder(c.config, &fakeNodeInfo, dokodemoTag)
	if err != nil {

		return err
	}
	err = c.addOutbound(outBoundConfig)
	if err != nil {

		return err
	}
	return nil
}

func (c *Controller) addNewUser(userInfo *[]api.UserInfo, nodeInfo *api.NodeInfo) (err error) {
	users := make([]*protocol.User, 0)
	switch nodeInfo.NodeType {
	case "Vless":
		users = c.buildVlessUser(userInfo, nodeInfo.Flow)
	case "Vmess":
		users = c.buildVmessUser(userInfo, nodeInfo.AlterID)	
	case "Trojan":
		users = c.buildTrojanUser(userInfo)
	case "Shadowsocks":
		users = c.buildSSUser(userInfo, nodeInfo.CypherMethod)
	case "Shadowsocks-Plugin":
		users = c.buildSSPluginUser(userInfo, nodeInfo.CypherMethod)
	default:
		return fmt.Errorf("unsupported node type: %s", nodeInfo.NodeType)
	}

	err = c.addUsers(users, c.Tag)
	if err != nil {
		return err
	}
	log.Printf("%s Added %d new users", c.logPrefix(), len(*userInfo))
	return nil
}

func compareUserList(old, new *[]api.UserInfo) (deleted, added []api.UserInfo) {
	mSrc := make(map[api.UserInfo]byte) // 按源数组建索引
	mAll := make(map[api.UserInfo]byte) // 源+目所有元素建索引

	var set []api.UserInfo // 交集

	// 1.源数组建立map
	for _, v := range *old {
		mSrc[v] = 0
		mAll[v] = 0
	}
	// 2.目数组中，存不进去，即重复元素，所有存不进去的集合就是并集
	for _, v := range *new {
		l := len(mAll)
		mAll[v] = 1
		if l != len(mAll) { // 长度变化，即可以存
			l = len(mAll)
		} else { // 存不了，进并集
			set = append(set, v)
		}
	}
	// 3.遍历交集，在并集中找，找到就从并集中删，删完后就是补集（即并-交=所有变化的元素）
	for _, v := range set {
		delete(mAll, v)
	}
	// 4.此时，mall是补集，所有元素去源中找，找到就是删除的，找不到的必定能在目数组中找到，即新加的
	for v := range mAll {
		_, exist := mSrc[v]
		if exist {
			deleted = append(deleted, v)
		} else {
			added = append(added, v)
		}
	}

	return deleted, added
}

func (c *Controller) userInfoMonitor() (err error) {

	// Get server status
	CPU, Mem, Disk, Uptime, err := serverstatus.GetSystemInfo()
	if err != nil {
		log.Print(err)
	}
	err = c.apiClient.ReportNodeStatus(
		&api.NodeStatus{
			CPU:    CPU,
			Mem:    Mem,
			Disk:   Disk,
			Uptime: Uptime,
		})
	if err != nil {
		log.Print(err)
	}

	// Get User traffic
	var userTraffic []api.UserTraffic
	var upCounterList []stats.Counter
	var downCounterList []stats.Counter

	for _, user := range *c.userList {
		up, down, upCounter, downCounter := c.getTraffic(c.buildUserTag(&user))
		if up > 0 || down > 0 {
			userTraffic = append(userTraffic, api.UserTraffic{
				UID:      user.UID,
				Email:    user.Email,
				Upload:   up,
				Download: down})

			if upCounter != nil {
				upCounterList = append(upCounterList, upCounter)
			}
			if downCounter != nil {
				downCounterList = append(downCounterList, downCounter)
			}
		}
	}

	if len(userTraffic) > 0 {
		var err error // Define an empty error

		err = c.apiClient.ReportUserTraffic(&userTraffic)
		// If report traffic error, not clear the traffic
		if err != nil {
			log.Print(err)
		} else {
			c.resetTraffic(&upCounterList, &downCounterList)
		}
	}

	// Report Online info
	if onlineDevice, err := c.GetOnlineDevice(c.Tag); err != nil {
		log.Print(err)
	} else if len(*onlineDevice) > 0 {
		if err = c.apiClient.ReportNodeOnlineUsers(onlineDevice); err != nil {
			log.Print(err)
		} else {
			log.Printf("%s Report %d online IPs", c.logPrefix(), len(*onlineDevice))
		}
	}

	// Report Illegal user
	if detectResult, err := c.GetDetectResult(c.Tag); err != nil {
		log.Print(err)
	} else if len(*detectResult) > 0 {
		if err = c.apiClient.ReportIllegal(detectResult); err != nil {
			log.Print(err)
		} else {
			log.Printf("%s Report %d operations blocked by detection rules", c.logPrefix(), len(*detectResult))
		}

	}
	return nil
}

func (c *Controller) buildNodeTag() string {
	return fmt.Sprintf("%s_%d_%d", c.nodeInfo.NodeType, c.nodeInfo.Port, c.nodeInfo.NodeID)
}

func (c *Controller) buildRNodeTag() string {
	return fmt.Sprintf("Relay_%s_%d_%d", c.relaynodeInfo.NodeType, c.relaynodeInfo.Port, c.relaynodeInfo.NodeID)
}

func (c *Controller) logPrefix() string {
	return fmt.Sprintf("[%s] %s(NodeID=%d)", c.clientInfo.APIHost, c.nodeInfo.NodeType, c.nodeInfo.NodeID)
}

// Check Cert
func (c *Controller) certMonitor() error {
	if c.nodeInfo.EnableTLS {
		switch c.nodeInfo.CertMode {
		case "dns", "http", "tls":
			lego, err := mylego.New(c.config.CertConfig)
			if err != nil {
				log.Print(err)
			}
			// Xray-core supports the OcspStapling certification hot renew
			_, _, _, err = lego.RenewCert(c.nodeInfo.CertMode, c.nodeInfo.CertDomain)
			if err != nil {
				log.Print(err)
			}
		}
	}
	return nil
}

// append remote dns
func (c *Controller) addNewDNS(newNodeInfo *api.NodeInfo) error {
	// reserve local DNS
	servers := c.config.DNSConfig.Servers
	servers = append(servers, newNodeInfo.NameServerConfig...)
	dns := conf.DNSConfig{
		Servers:                servers,
		Hosts:                  c.config.DNSConfig.Hosts,
		ClientIP:               c.config.DNSConfig.ClientIP,
		Tag:                    c.config.DNSConfig.Tag,
		QueryStrategy:          c.config.DNSConfig.QueryStrategy,
		DisableCache:           c.config.DNSConfig.DisableCache,
		DisableFallback:        c.config.DNSConfig.DisableFallback,
		DisableFallbackIfMatch: c.config.DNSConfig.DisableFallbackIfMatch,
	}

	dnsConfig, err := dns.Build()
	if err != nil {
		log.Panicf("Failed to understand DNS config, Please check: https://xtls.github.io/config/dns.html for help: %s", err)
	}
	dnsInstance, err := serial.ToTypedMessage(dnsConfig).GetInstance()
	if err != nil {
		return err
	}
	obj, err := core.CreateObject(c.server, dnsInstance)
	if err != nil {
		return err
	}
	if feature, ok := obj.(features.Feature); ok {
		if err := c.server.AddFeature(feature); err != nil {
			return err
		}
	}

	return nil
}