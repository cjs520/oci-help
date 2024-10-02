package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"io/ioutil"
	"log"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-ini/ini"
	"github.com/oracle/oci-go-sdk/v65/usageapi"

	"github.com/oracle/oci-go-sdk/v65/common"
	"github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/example/helpers"
	"github.com/oracle/oci-go-sdk/v65/identity"
)

const (
	defConfigFilePath = "./oci-help.ini"
	IPsFilePrefix     = "IPs"
)

var (
	instanceMutex       sync.Mutex
	lastCallbackID      string
	callbackMutex       sync.Mutex
	bot                 *tgbotapi.BotAPI
	configFilePath      string
	provider            common.ConfigurationProvider
	computeClient       core.ComputeClient
	networkClient       core.VirtualNetworkClient
	storageClient       core.BlockstorageClient
	identityClient      identity.IdentityClient
	ctx                 context.Context = context.Background()
	oracleSections      []*ini.Section
	oracleSection       *ini.Section
	oracleSectionName   string
	oracle              Oracle
	instanceBaseSection *ini.Section
	instance            Instance
	proxy               string
	token               string
	chat_id             string
	cmd                 string
	sendMessageUrl      string
	editMessageUrl      string
	EACH                bool
	availabilityDomains []identity.AvailabilityDomain
)

type Oracle struct {
	User         string `ini:"user"`
	Fingerprint  string `ini:"fingerprint"`
	Tenancy      string `ini:"tenancy"`
	Region       string `ini:"region"`
	Key_file     string `ini:"key_file"`
	Key_password string `ini:"key_password"`
}

type Instance struct {
	AvailabilityDomain     string  `ini:"availabilityDomain"`
	SSH_Public_Key         string  `ini:"ssh_authorized_key"`
	VcnDisplayName         string  `ini:"vcnDisplayName"`
	SubnetDisplayName      string  `ini:"subnetDisplayName"`
	Shape                  string  `ini:"shape"`
	OperatingSystem        string  `ini:"OperatingSystem"`
	OperatingSystemVersion string  `ini:"OperatingSystemVersion"`
	InstanceDisplayName    string  `ini:"instanceDisplayName"`
	Ocpus                  float32 `ini:"cpus"`
	MemoryInGBs            float32 `ini:"memoryInGBs"`
	Burstable              string  `ini:"burstable"`
	BootVolumeSizeInGBs    int64   `ini:"bootVolumeSizeInGBs"`
	Sum                    int32   `ini:"sum"`
	Each                   int32   `ini:"each"`
	Retry                  int32   `ini:"retry"`
	CloudInit              string  `ini:"cloud-init"`
	MinTime                int32   `ini:"minTime"`
	MaxTime                int32   `ini:"maxTime"`
}

type Message struct {
	OK          bool `json:"ok"`
	Result      `json:"result"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
}
type Result struct {
	MessageId int `json:"message_id"`
}

func main() {
	flag.StringVar(&configFilePath, "config", defConfigFilePath, "配置文件路径")
	flag.StringVar(&configFilePath, "c", defConfigFilePath, "配置文件路径")
	flag.Parse()

	cfg, err := ini.Load(configFilePath)
	helpers.FatalIfError(err)

	defSec := cfg.Section(ini.DefaultSection)
	proxy = defSec.Key("proxy").Value()
	token = defSec.Key("token").Value()
	chat_id = defSec.Key("chat_id").Value()
	cmd = defSec.Key("cmd").Value()
	if defSec.HasKey("EACH") {
		EACH, _ = defSec.Key("EACH").Bool()
	} else {
		EACH = true
	}
	sendMessageUrl = "https://api.telegram.org/bot" + token + "/sendMessage"
	editMessageUrl = "https://api.telegram.org/bot" + token + "/editMessageText"
	rand.Seed(time.Now().UnixNano())

	sections := cfg.Sections()
	oracleSections = []*ini.Section{}
	for _, sec := range sections {
		if len(sec.ParentKeys()) == 0 {
			user := sec.Key("user").Value()
			fingerprint := sec.Key("fingerprint").Value()
			tenancy := sec.Key("tenancy").Value()
			region := sec.Key("region").Value()
			key_file := sec.Key("key_file").Value()
			if user != "" && fingerprint != "" && tenancy != "" && region != "" && key_file != "" {
				oracleSections = append(oracleSections, sec)
			}
		}
	}
	if len(oracleSections) == 0 {
		log.Fatalf("未找到正确的配置信息, 请参考链接文档配置相关信息。链接: https://github.com/lemoex/oci-help")
	}
	instanceBaseSection = cfg.Section("INSTANCE")

	bot, err = tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Panic(err)
	}

	log.Printf("Authorized on account %s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := bot.GetUpdatesChan(u)

	for update := range updates {
		if update.CallbackQuery != nil {
			handleCallback(update.CallbackQuery)
		} else if update.Message != nil {
			handleMessage(update.Message)
		}
	}
}

func handleMessage(message *tgbotapi.Message) {
	if message.IsCommand() {
		switch message.Command() {
		case "start":
			sendMainMenu(message.Chat.ID)
		default:
			msg := tgbotapi.NewMessage(message.Chat.ID, "未知命令，请使用 /start 开始")
			bot.Send(msg)
		}
	} else if message.ReplyToMessage != nil {
		state, exists := getUserState(message.Chat.ID)
		if exists {
			switch state.Action {
			case "resizing_boot_volume":
				handleResizeBootVolume(message.Chat.ID, state.InstanceIndex, message.Text)
			}
			clearUserState(message.Chat.ID)
		}
	}

}
func handleResizeBootVolume(chatID int64, volumeIndex int, sizeText string) {
	size, err := strconv.ParseInt(sizeText, 10, 64)
	if err != nil {
		sendErrorMessage(chatID, "输入的大小无效，请输入一个整数")
		return
	}

	var bootVolumes []core.BootVolume
	for _, ad := range availabilityDomains {
		volumes, _ := getBootVolumes(ad.Name)
		bootVolumes = append(bootVolumes, volumes...)
	}

	if volumeIndex < 0 || volumeIndex >= len(bootVolumes) {
		sendErrorMessage(chatID, "无效的引导卷索引")
		return
	}

	volume := bootVolumes[volumeIndex]
	_, err = updateBootVolume(volume.Id, &size, nil)
	if err != nil {
		sendErrorMessage(chatID, "调整引导卷大小失败: "+err.Error())
	} else {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("引导卷 '%s' 的大小已成功调整为 %d GB", *volume.DisplayName, size))
		bot.Send(msg)
	}

	// 调整后，重新显示引导卷详情
	manageBootVolumesTelegram(chatID)
}
func updateNewInstance(newInstance Instance) {
	instanceMutex.Lock()
	defer instanceMutex.Unlock()
	instance = newInstance
}
func getCurrentRenamingInstanceIndex(chatID int64) int {
	state, exists := getUserState(chatID)
	if !exists || state.Action != "renaming" {
		return -1 // 表示没有正在进行的重命名操作
	}
	return state.InstanceIndex
}
func setUserState(chatID int64, action string, instanceIndex int) {
	stateMutex.Lock()
	defer stateMutex.Unlock()
	userStates[chatID] = UserState{
		Action:        action,
		InstanceIndex: instanceIndex,
	}
}

type UserState struct {
	Action        string // 例如 "renaming", "upgrading"
	InstanceIndex int
}

var (
	userStates = make(map[int64]UserState)
	stateMutex sync.Mutex
)

func getUserState(chatID int64) (UserState, bool) {
	stateMutex.Lock()
	defer stateMutex.Unlock()
	state, exists := userStates[chatID]
	return state, exists
}

func clearUserState(chatID int64) {
	stateMutex.Lock()
	defer stateMutex.Unlock()
	delete(userStates, chatID)
}
func getCurrentUpgradingInstanceIndex(chatID int64) int {
	state, exists := getUserState(chatID)
	if !exists || state.Action != "upgrading" {
		return -1 // 表示没有正在进行的升级操作
	}
	return state.InstanceIndex
}
func getInstanceCopy() Instance {
	instanceMutex.Lock()
	defer instanceMutex.Unlock()
	return instance
}
func terminateInstanceAction(chatID int64, instanceIndex int) {
	instances, _, err := ListInstances(ctx, computeClient, nil)
	if err != nil || instanceIndex >= len(instances) {
		sendErrorMessage(chatID, "获取实例信息失败或实例索引无效")
		return
	}

	instance := instances[instanceIndex]
	err = terminateInstance(instance.Id)
	if err != nil {
		sendErrorMessage(chatID, "终止实例失败: "+err.Error())
	} else {
		msg := tgbotapi.NewMessage(chatID, "正在终止实例，请稍后查看实例状态")
		bot.Send(msg)
	}
}

func changePublicIpAction(chatID int64, instanceIndex int) {
	instances, _, err := ListInstances(ctx, computeClient, nil)
	if err != nil || instanceIndex >= len(instances) {
		sendErrorMessage(chatID, "获取实例信息失败或实例索引无效")
		return
	}

	instance := instances[instanceIndex]
	vnics, err := getInstanceVnics(instance.Id)
	if err != nil {
		sendErrorMessage(chatID, "获取实例VNIC失败: "+err.Error())
		return
	}

	publicIp, err := changePublicIp(vnics)
	if err != nil {
		sendErrorMessage(chatID, "更换公共IP失败: "+err.Error())
	} else {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("更换公共IP成功，新的IP地址: %s", *publicIp.IpAddress))
		bot.Send(msg)
	}
}

func configureAgentAction(chatID int64, instanceIndex int, action string) {
	instances, _, err := ListInstances(ctx, computeClient, nil)
	if err != nil || instanceIndex >= len(instances) {
		sendErrorMessage(chatID, "获取实例信息失败或实例索引无效")
		return
	}

	instance := instances[instanceIndex]
	var disable bool
	if action == "disable" {
		disable = true
	} else {
		disable = false
	}

	_, err = updateInstance(instance.Id, nil, nil, nil, instance.AgentConfig.PluginsConfig, &disable)
	if err != nil {
		sendErrorMessage(chatID, fmt.Sprintf("%s管理和监控插件失败: %s", action, err.Error()))
	} else {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("%s管理和监控插件成功", action))
		bot.Send(msg)
	}
}

func handleCallback(callback *tgbotapi.CallbackQuery) {
	callbackMutex.Lock()
	defer callbackMutex.Unlock()

	// 检查是否是重复的回调
	if callback.ID == lastCallbackID {
		return
	}
	lastCallbackID = callback.ID

	data := callback.Data
	chatID := callback.Message.Chat.ID

	// 处理特定前缀的回调
	if handled := handlePrefixedCallbacks(data, chatID); handled {
		return
	}

	// 处理特定的回调数据
	switch data {
	case "list_accounts":
		sendAccountList(chatID)
	case "main_menu":
		sendMainMenu(chatID)
	case "confirm_create_instance":
		startCreateInstance(chatID)
	default:
		handleRemainingCallbacks(data, chatID)
	}
}
func handleDetachBootVolume(chatID int64, volumeIndex int) {
	var bootVolumes []core.BootVolume
	for _, ad := range availabilityDomains {
		volumes, _ := getBootVolumes(ad.Name)
		bootVolumes = append(bootVolumes, volumes...)
	}

	if volumeIndex < 0 || volumeIndex >= len(bootVolumes) {
		sendErrorMessage(chatID, "无效的引导卷索引")
		return
	}

	volume := bootVolumes[volumeIndex]
	attachments, err := listBootVolumeAttachments(volume.AvailabilityDomain, volume.CompartmentId, volume.Id)
	if err != nil {
		sendErrorMessage(chatID, "获取引导卷附件失败: "+err.Error())
		return
	}

	for _, attachment := range attachments {
		_, err := detachBootVolume(attachment.Id)
		if err != nil {
			sendErrorMessage(chatID, "分离引导卷失败: "+err.Error())
		} else {
			msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("已成功分离引导卷 '%s'", *volume.DisplayName))
			bot.Send(msg)
		}
	}

	// 分离后，重新显示引导卷详情
	manageBootVolumesTelegram(chatID)

}

func handleTerminateBootVolume(chatID int64, volumeIndex int) {
	var bootVolumes []core.BootVolume
	for _, ad := range availabilityDomains {
		volumes, _ := getBootVolumes(ad.Name)
		bootVolumes = append(bootVolumes, volumes...)
	}

	if volumeIndex < 0 || volumeIndex >= len(bootVolumes) {
		sendErrorMessage(chatID, "无效的引导卷索引")
		return
	}

	volume := bootVolumes[volumeIndex]
	_, err := deleteBootVolume(volume.Id)
	if err != nil {
		sendErrorMessage(chatID, "终止引导卷失败: "+err.Error())
	} else {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("已成功终止引导卷 '%s'", *volume.DisplayName))
		bot.Send(msg)
	}

	// 终止后，返回到引导卷列表
	manageBootVolumesTelegram(chatID)
}
func handleBootVolumePerformance(chatID int64, volumeIndex int, performance int64) {
	var bootVolumes []core.BootVolume
	for _, ad := range availabilityDomains {
		volumes, _ := getBootVolumes(ad.Name)
		bootVolumes = append(bootVolumes, volumes...)
	}

	if volumeIndex < 0 || volumeIndex >= len(bootVolumes) {
		sendErrorMessage(chatID, "无效的引导卷索引")
		return
	}

	volume := bootVolumes[volumeIndex]
	_, err := updateBootVolume(volume.Id, nil, &performance)
	if err != nil {
		sendErrorMessage(chatID, "调整引导卷性能失败: "+err.Error())
	} else {
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("引导卷 '%s' 的性能已成功调整为 %d VPUs/GB", *volume.DisplayName, performance))
		bot.Send(msg)
	}

	// 调整后，重新显示引导卷详情
	manageBootVolumesTelegram(chatID)
}
func handlePrefixedCallbacks(data string, chatID int64) bool {
	switch {
	case strings.HasPrefix(data, "create_instance:"):
		index, _ := strconv.Atoi(strings.TrimPrefix(data, "create_instance:"))
		confirmCreateInstance(chatID, index)
	case strings.HasPrefix(data, "instance_action:"):
		parts := strings.Split(data, ":")
		if len(parts) == 3 {
			instanceIndex, _ := strconv.Atoi(parts[1])
			action := parts[2]
			handleInstanceAction(chatID, instanceIndex, action)
		}
	case strings.HasPrefix(data, "confirm_terminate:"):
		instanceIndex, _ := strconv.Atoi(strings.TrimPrefix(data, "confirm_terminate:"))
		terminateInstanceAction(chatID, instanceIndex)
	case strings.HasPrefix(data, "confirm_change_ip:"):
		instanceIndex, _ := strconv.Atoi(strings.TrimPrefix(data, "confirm_change_ip:"))
		changePublicIpAction(chatID, instanceIndex)
	case strings.HasPrefix(data, "agent_config:"):
		parts := strings.Split(data, ":")
		if len(parts) == 3 {
			instanceIndex, _ := strconv.Atoi(parts[1])
			action := parts[2]
			configureAgentAction(chatID, instanceIndex, action)
		}
	case strings.HasPrefix(data, "boot_volume_performance:"):
		parts := strings.Split(data, ":")
		if len(parts) == 3 {
			volumeIndex, _ := strconv.Atoi(parts[1])
			performance, _ := strconv.ParseInt(parts[2], 10, 64)
			handleBootVolumePerformance(chatID, volumeIndex, performance)
		}
	case strings.HasPrefix(data, "confirm_terminate_boot_volume:"):
		volumeIndex, _ := strconv.Atoi(strings.TrimPrefix(data, "confirm_terminate_boot_volume:"))
		handleTerminateBootVolume(chatID, volumeIndex)
	default:
		return false
	}
	return true
}

func handleRemainingCallbacks(data string, chatID int64) {
	switch {
	case strings.HasPrefix(data, "select_account:"):
		accountIndex, _ := strconv.Atoi(strings.TrimPrefix(data, "select_account:"))
		selectAccount(chatID, accountIndex)
	case strings.HasPrefix(data, "account_action:"):
		action := strings.TrimPrefix(data, "account_action:")
		handleAccountAction(chatID, action)
	case strings.HasPrefix(data, "instance_details:"):
		instanceIndex, _ := strconv.Atoi(strings.TrimPrefix(data, "instance_details:"))
		showInstanceDetails(chatID, instanceIndex)
	case strings.HasPrefix(data, "boot_volume_details:"):
		volumeIndex, _ := strconv.Atoi(strings.TrimPrefix(data, "boot_volume_details:"))
		showBootVolumeDetails(chatID, volumeIndex)
	case strings.HasPrefix(data, "boot_volume_action:"):
		parts := strings.Split(data, ":")
		if len(parts) == 3 {
			volumeIndex, _ := strconv.Atoi(parts[1])
			action := parts[2]
			handleBootVolumeAction(chatID, volumeIndex, action)
		}
	default:
		log.Printf("未知的回调数据: %s", data)
	}
}
func viewCostTelegram(chatID int64) {
	msg := tgbotapi.NewMessage(chatID, "正在获取成本数据...")
	sentMsg, _ := bot.Send(msg)

	usageapiClient, err := usageapi.NewUsageapiClientWithConfigurationProvider(provider)
	if err != nil {
		editMsg := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, "创建 UsageapiClient 失败: "+err.Error())
		bot.Send(editMsg)
		return
	}

	firstDay, lastDay := currMouthFirstLastDay()
	tenancyOCID, err := provider.TenancyOCID()
	if err != nil {
		editMsg := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, "获取 Tenancy OCID 失败: "+err.Error())
		bot.Send(editMsg)
		return
	}

	req := usageapi.RequestSummarizedUsagesRequest{
		RequestSummarizedUsagesDetails: usageapi.RequestSummarizedUsagesDetails{
			CompartmentDepth: common.Float32(6),
			Granularity:      usageapi.RequestSummarizedUsagesDetailsGranularityMonthly,
			GroupBy:          []string{"service"},
			QueryType:        usageapi.RequestSummarizedUsagesDetailsQueryTypeUsage,
			TenantId:         &tenancyOCID,
			TimeUsageStarted: &common.SDKTime{Time: firstDay},
			TimeUsageEnded:   &common.SDKTime{Time: lastDay},
		},
	}

	resp, err := usageapiClient.RequestSummarizedUsages(context.Background(), req)
	if err != nil {
		editMsg := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, "获取成本数据失败: "+err.Error())
		bot.Send(editMsg)
		return
	}

	var messageText strings.Builder
	messageText.WriteString("本月成本概览：\n\n")

	var totalCost float32
	for _, item := range resp.Items {
		if item.Service == nil || item.Unit == nil || item.ComputedAmount == nil || item.ComputedQuantity == nil {
			continue // 跳过无效的数据
		}
		cost := *item.ComputedAmount
		totalCost += cost
		messageText.WriteString(fmt.Sprintf("[服务: %s] 单位: %s 费用: %.2f 使用量: %.2f\n",
			*item.Service, *item.Unit, *item.ComputedAmount, *item.ComputedQuantity))
	}

	messageText.WriteString(fmt.Sprintf("\n总成本: %.2f\n", totalCost))

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("返回", "select_account:"+strconv.Itoa(getCurrentAccountIndex())),
		),
	)

	editMsg := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, messageText.String())
	editMsg.ReplyMarkup = &keyboard
	bot.Send(editMsg)
}

func showBootVolumeDetails(chatID int64, volumeIndex int) {
	var bootVolumes []core.BootVolume
	for _, ad := range availabilityDomains {
		volumes, _ := getBootVolumes(ad.Name)
		bootVolumes = append(bootVolumes, volumes...)
	}

	if volumeIndex < 0 || volumeIndex >= len(bootVolumes) {
		msg := tgbotapi.NewMessage(chatID, "无效的引导卷索引")
		bot.Send(msg)
		return
	}

	volume := bootVolumes[volumeIndex]

	attachments, _ := listBootVolumeAttachments(volume.AvailabilityDomain, volume.CompartmentId, volume.Id)
	attachIns := make([]string, 0)
	for _, attachment := range attachments {
		ins, err := getInstance(attachment.InstanceId)
		if err != nil {
			attachIns = append(attachIns, err.Error())
		} else {
			attachIns = append(attachIns, *ins.DisplayName)
		}
	}

	var performance string
	switch *volume.VpusPerGB {
	case 10:
		performance = fmt.Sprintf("均衡 (VPU:%d)", *volume.VpusPerGB)
	case 20:
		performance = fmt.Sprintf("性能较高 (VPU:%d)", *volume.VpusPerGB)
	default:
		performance = fmt.Sprintf("UHP (VPU:%d)", *volume.VpusPerGB)
	}

	var messageText strings.Builder
	messageText.WriteString(fmt.Sprintf("引导卷详情：\n\n"))
	messageText.WriteString(fmt.Sprintf("名称: %s\n", *volume.DisplayName))
	messageText.WriteString(fmt.Sprintf("状态: %s\n", getBootVolumeState(volume.LifecycleState)))
	messageText.WriteString(fmt.Sprintf("OCID: %s\n", *volume.Id))
	messageText.WriteString(fmt.Sprintf("大小: %d GB\n", *volume.SizeInGBs))
	messageText.WriteString(fmt.Sprintf("可用性域: %s\n", *volume.AvailabilityDomain))
	messageText.WriteString(fmt.Sprintf("性能: %s\n", performance))
	messageText.WriteString(fmt.Sprintf("附加的实例: %s\n", strings.Join(attachIns, ", ")))

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("修改性能", fmt.Sprintf("boot_volume_action:%d:performance", volumeIndex)),
			tgbotapi.NewInlineKeyboardButtonData("修改大小", fmt.Sprintf("boot_volume_action:%d:resize", volumeIndex)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("分离引导卷", fmt.Sprintf("boot_volume_action:%d:detach", volumeIndex)),
			tgbotapi.NewInlineKeyboardButtonData("终止引导卷", fmt.Sprintf("boot_volume_action:%d:terminate", volumeIndex)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("返回引导卷列表", "account_action:manage_boot_volumes"),
		),
	)

	msg := tgbotapi.NewMessage(chatID, messageText.String())
	msg.ReplyMarkup = keyboard
	bot.Send(msg)
}

func handleBootVolumeAction(chatID int64, volumeIndex int, action string) {
	var bootVolumes []core.BootVolume
	for _, ad := range availabilityDomains {
		volumes, _ := getBootVolumes(ad.Name)
		bootVolumes = append(bootVolumes, volumes...)
	}

	if volumeIndex < 0 || volumeIndex >= len(bootVolumes) {
		msg := tgbotapi.NewMessage(chatID, "无效的引导卷索引")
		bot.Send(msg)
		return
	}

	volume := bootVolumes[volumeIndex]

	switch action {
	case "performance":
		keyboard := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("均衡", fmt.Sprintf("boot_volume_performance:%d:10", volumeIndex)),
				tgbotapi.NewInlineKeyboardButtonData("高性能", fmt.Sprintf("boot_volume_performance:%d:20", volumeIndex)),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("返回", fmt.Sprintf("boot_volume_details:%d", volumeIndex)),
			),
		)
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("当前引导卷性能：%d VPUs/GB\n请选择新的引导卷性能：", *volume.VpusPerGB))
		msg.ReplyMarkup = keyboard
		bot.Send(msg)
	case "resize":
		msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("当前引导卷大小：%d GB\n请输入新的引导卷大小（GB）：", *volume.SizeInGBs))
		msg.ReplyMarkup = tgbotapi.ForceReply{ForceReply: true, Selective: true}
		bot.Send(msg)
		setUserState(chatID, "resizing_boot_volume", volumeIndex)
	case "detach":
		confirmDetachBootVolume(chatID, volumeIndex)
	case "terminate":
		confirmTerminateBootVolume(chatID, volumeIndex)
	default:
		msg := tgbotapi.NewMessage(chatID, "未知的操作")
		bot.Send(msg)
	}
}
func confirmDetachBootVolume(chatID int64, volumeIndex int) {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("确认分离", fmt.Sprintf("confirm_detach_boot_volume:%d", volumeIndex)),
			tgbotapi.NewInlineKeyboardButtonData("取消", fmt.Sprintf("boot_volume_details:%d", volumeIndex)),
		),
	)
	msg := tgbotapi.NewMessage(chatID, "确定要分离此引导卷吗？")
	msg.ReplyMarkup = keyboard
	bot.Send(msg)
}

func confirmTerminateBootVolume(chatID int64, volumeIndex int) {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("确认终止", fmt.Sprintf("confirm_terminate_boot_volume:%d", volumeIndex)),
			tgbotapi.NewInlineKeyboardButtonData("取消", fmt.Sprintf("boot_volume_details:%d", volumeIndex)),
		),
	)
	msg := tgbotapi.NewMessage(chatID, "确定要终止此引导卷吗？此操作不可逆。")
	msg.ReplyMarkup = keyboard
	bot.Send(msg)
}
func manageBootVolumesTelegram(chatID int64) {
	msg := tgbotapi.NewMessage(chatID, "正在获取引导卷数据...")
	sentMsg, _ := bot.Send(msg)

	var bootVolumes []core.BootVolume
	var wg sync.WaitGroup
	var mu sync.Mutex // 用于保护 bootVolumes 切片
	errorChan := make(chan error, len(availabilityDomains))

	for _, ad := range availabilityDomains {
		wg.Add(1)
		go func(adName *string) {
			defer wg.Done()
			volumes, err := getBootVolumes(adName)
			if err != nil {
				errorChan <- fmt.Errorf("获取可用性域 %s 的引导卷失败: %v", *adName, err)
			} else {
				mu.Lock()
				bootVolumes = append(bootVolumes, volumes...)
				mu.Unlock()
			}
		}(ad.Name)
	}
	wg.Wait()
	close(errorChan)

	// 收集所有错误
	var errorMessages []string
	for err := range errorChan {
		errorMessages = append(errorMessages, err.Error())
	}

	if len(bootVolumes) == 0 {
		var messageText string
		if len(errorMessages) > 0 {
			messageText = fmt.Sprintf("获取引导卷时出现错误:\n%s\n\n没有找到任何引导卷。", strings.Join(errorMessages, "\n"))
		} else {
			messageText = "没有找到任何引导卷。可能是因为当前账户下没有创建引导卷，或所有引导卷都已被附加到实例上。"
		}
		editMsg := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, messageText)
		bot.Send(editMsg)

		// 添加一个返回按钮
		keyboard := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("返回", "select_account:"+strconv.Itoa(getCurrentAccountIndex())),
			),
		)
		editMsg.ReplyMarkup = &keyboard
		bot.Send(editMsg)
		return
	}

	// 剩余的代码保持不变
	var messageText strings.Builder
	messageText.WriteString(fmt.Sprintf("引导卷 (当前账号: %s)\n\n", oracleSection.Name()))
	messageText.WriteString(fmt.Sprintf("%-5s %-30s %-15s %-10s\n", "序号", "名称", "状态", "大小(GB)"))

	var keyboard [][]tgbotapi.InlineKeyboardButton

	for i, volume := range bootVolumes {
		messageText.WriteString(fmt.Sprintf("%-5d %-30s %-15s %-10d\n",
			i+1,
			*volume.DisplayName,
			getBootVolumeState(volume.LifecycleState),
			*volume.SizeInGBs))

		button := tgbotapi.NewInlineKeyboardButtonData(
			fmt.Sprintf("引导卷 %d", i+1),
			fmt.Sprintf("boot_volume_details:%d", i))
		row := tgbotapi.NewInlineKeyboardRow(button)
		keyboard = append(keyboard, row)
	}

	keyboard = append(keyboard, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("返回", "select_account:"+strconv.Itoa(getCurrentAccountIndex())),
	))

	editMsg := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, messageText.String())
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: keyboard}
	bot.Send(editMsg)
}
func handleInstanceAction(chatID int64, instanceIndex int, action string) {
	instances, _, err := ListInstances(ctx, computeClient, nil)
	if err != nil || instanceIndex >= len(instances) {
		sendErrorMessage(chatID, "获取实例信息失败或实例索引无效")
		return
	}

	instance := instances[instanceIndex]

	switch action {
	case "start":
		_, err := instanceAction(instance.Id, core.InstanceActionActionStart)
		sendActionResult(chatID, "启动实例", err)
	case "stop":
		_, err := instanceAction(instance.Id, core.InstanceActionActionSoftstop)
		sendActionResult(chatID, "停止实例", err)
	case "reset":
		_, err := instanceAction(instance.Id, core.InstanceActionActionSoftreset)
		sendActionResult(chatID, "重启实例", err)
	case "terminate":
		confirmTerminateInstance(chatID, instanceIndex)
	case "change_ip":
		confirmChangePublicIp(chatID, instanceIndex)
	case "agent_config":
		promptAgentConfig(chatID, instanceIndex)
	default:
		sendErrorMessage(chatID, "未知的实例操作")
	}
}
func sendActionResult(chatID int64, action string, err error) {
	var message string
	if err != nil {
		message = fmt.Sprintf("%s失败: %s", action, err.Error())
	} else {
		message = fmt.Sprintf("%s成功，请稍后查看实例状态", action)
	}
	msg := tgbotapi.NewMessage(chatID, message)
	bot.Send(msg)
}
func sendErrorMessage(chatID int64, message string) {
	msg := tgbotapi.NewMessage(chatID, "错误: "+message)
	bot.Send(msg)
}

func confirmTerminateInstance(chatID int64, instanceIndex int) {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("确认终止", fmt.Sprintf("confirm_terminate:%d", instanceIndex)),
			tgbotapi.NewInlineKeyboardButtonData("取消", fmt.Sprintf("instance_details:%d", instanceIndex)),
		),
	)
	msg := tgbotapi.NewMessage(chatID, "您确定要终止此实例吗？此操作不可逆。")
	msg.ReplyMarkup = keyboard
	bot.Send(msg)
}

func confirmChangePublicIp(chatID int64, instanceIndex int) {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("确认更换", fmt.Sprintf("confirm_change_ip:%d", instanceIndex)),
			tgbotapi.NewInlineKeyboardButtonData("取消", fmt.Sprintf("instance_details:%d", instanceIndex)),
		),
	)
	msg := tgbotapi.NewMessage(chatID, "确定要更换此实例的公共IP吗？这将删除当前的公共IP并创建一个新的。")
	msg.ReplyMarkup = keyboard
	bot.Send(msg)
}

func promptAgentConfig(chatID int64, instanceIndex int) {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("启用插件", fmt.Sprintf("agent_config:%d:enable", instanceIndex)),
			tgbotapi.NewInlineKeyboardButtonData("禁用插件", fmt.Sprintf("agent_config:%d:disable", instanceIndex)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("返回", fmt.Sprintf("instance_details:%d", instanceIndex)),
		),
	)
	msg := tgbotapi.NewMessage(chatID, "请选择 Oracle Cloud Agent 插件配置:")
	msg.ReplyMarkup = keyboard
	bot.Send(msg)
}
func showInstanceDetails(chatID int64, instanceIndex int) {
	msg := tgbotapi.NewMessage(chatID, "正在获取实例详细信息...")
	sentMsg, _ := bot.Send(msg)

	instances, _, err := ListInstances(ctx, computeClient, nil)
	if err != nil || instanceIndex >= len(instances) {
		editMsg := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, "获取实例信息失败或实例索引无效")
		bot.Send(editMsg)
		return
	}

	instance := instances[instanceIndex]
	vnics, err := getInstanceVnics(instance.Id)
	if err != nil {
		editMsg := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, "获取实例VNIC失败: "+err.Error())
		bot.Send(editMsg)
		return
	}

	var publicIps []string
	for _, vnic := range vnics {
		if vnic.PublicIp != nil {
			publicIps = append(publicIps, *vnic.PublicIp)
		}
	}
	strPublicIps := strings.Join(publicIps, ", ")

	var messageText strings.Builder
	messageText.WriteString(fmt.Sprintf("实例详细信息 (当前账号: %s)\n\n", oracleSectionName))
	messageText.WriteString(fmt.Sprintf("名称: %s\n", *instance.DisplayName))
	messageText.WriteString(fmt.Sprintf("状态: %s\n", getInstanceState(instance.LifecycleState)))
	messageText.WriteString(fmt.Sprintf("公共IP: %s\n", strPublicIps))
	messageText.WriteString(fmt.Sprintf("可用性域: %s\n", *instance.AvailabilityDomain))
	messageText.WriteString(fmt.Sprintf("配置: %s\n", *instance.Shape))
	messageText.WriteString(fmt.Sprintf("OCPU计数: %g\n", *instance.ShapeConfig.Ocpus))
	messageText.WriteString(fmt.Sprintf("网络带宽(Gbps): %g\n", *instance.ShapeConfig.NetworkingBandwidthInGbps))
	messageText.WriteString(fmt.Sprintf("内存(GB): %g\n\n", *instance.ShapeConfig.MemoryInGBs))
	messageText.WriteString("Oracle Cloud Agent 插件配置情况\n")
	messageText.WriteString(fmt.Sprintf("监控插件已禁用？: %t\n", *instance.AgentConfig.IsMonitoringDisabled))
	messageText.WriteString(fmt.Sprintf("管理插件已禁用？: %t\n", *instance.AgentConfig.IsManagementDisabled))
	messageText.WriteString(fmt.Sprintf("所有插件均已禁用？: %t\n", *instance.AgentConfig.AreAllPluginsDisabled))
	for _, value := range instance.AgentConfig.PluginsConfig {
		messageText.WriteString(fmt.Sprintf("%s: %s\n", *value.Name, value.DesiredState))
	}

	keyboard := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("启动", fmt.Sprintf("instance_action:%d:start", instanceIndex)),
			tgbotapi.NewInlineKeyboardButtonData("停止", fmt.Sprintf("instance_action:%d:stop", instanceIndex)),
			tgbotapi.NewInlineKeyboardButtonData("重启", fmt.Sprintf("instance_action:%d:reset", instanceIndex)),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("终止", fmt.Sprintf("instance_action:%d:terminate", instanceIndex)),
			tgbotapi.NewInlineKeyboardButtonData("更换公共IP", fmt.Sprintf("instance_action:%d:change_ip", instanceIndex)),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("Agent插件配置", fmt.Sprintf("instance_action:%d:agent_config", instanceIndex)),
		},
		{
			tgbotapi.NewInlineKeyboardButtonData("返回实例列表", "account_action:list_instances"),
		},
	}

	editMsg := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, messageText.String())
	editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: keyboard}
	bot.Send(editMsg)
}
func createInstanceTelegram(chatID int64) {
	msg := tgbotapi.NewMessage(chatID, "正在获取可用性域和实例模板...")
	sentMsg, _ := bot.Send(msg)

	// 获取可用性域
	var err error
	availabilityDomains, err = ListAvailabilityDomains()
	if err != nil {
		editMsg := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, "获取可用性域失败: "+err.Error())
		bot.Send(editMsg)
		return
	}

	if len(availabilityDomains) == 0 {
		editMsg := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, "没有可用的可用性域")
		bot.Send(editMsg)
		return
	}

	var instanceSections []*ini.Section
	instanceSections = append(instanceSections, instanceBaseSection.ChildSections()...)
	instanceSections = append(instanceSections, oracleSection.ChildSections()...)

	if len(instanceSections) == 0 {
		editMsg := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, "未找到实例模板")
		bot.Send(editMsg)
		return
	}

	var messageText strings.Builder
	messageText.WriteString(fmt.Sprintf("选择对应的实例模板开始创建实例 (当前账号: %s)\n\n", oracleSectionName))
	messageText.WriteString(fmt.Sprintf("%-5s %-20s %-10s %-10s\n", "序号", "配置", "CPU个数", "内存(GB)"))

	var keyboard [][]tgbotapi.InlineKeyboardButton

	for i, instanceSec := range instanceSections {
		cpu := instanceSec.Key("cpus").Value()
		if cpu == "" {
			cpu = "-"
		}
		memory := instanceSec.Key("memoryInGBs").Value()
		if memory == "" {
			memory = "-"
		}
		shape := instanceSec.Key("shape").Value()

		messageText.WriteString(fmt.Sprintf("%-5d %-20s %-10s %-10s\n", i+1, shape, cpu, memory))

		button := tgbotapi.NewInlineKeyboardButtonData(
			fmt.Sprintf("模板 %d", i+1),
			fmt.Sprintf("create_instance:%d", i))
		row := tgbotapi.NewInlineKeyboardRow(button)
		keyboard = append(keyboard, row)
	}

	keyboard = append(keyboard, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("返回", "select_account:"+strconv.Itoa(getCurrentAccountIndex())),
	))

	editMsg := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, messageText.String())
	editMsg.ParseMode = "Markdown"
	editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: keyboard}
	bot.Send(editMsg)
}

func confirmCreateInstance(chatID int64, index int) {
	var instanceSections []*ini.Section
	instanceSections = append(instanceSections, instanceBaseSection.ChildSections()...)
	instanceSections = append(instanceSections, oracleSection.ChildSections()...)

	if index < 0 || index >= len(instanceSections) {
		msg := tgbotapi.NewMessage(chatID, "无效的模板选择")
		bot.Send(msg)
		return
	}

	instanceSection := instanceSections[index]
	var newInstance Instance
	err := instanceSection.MapTo(&newInstance)
	if err != nil {
		msg := tgbotapi.NewMessage(chatID, "解析实例模板参数失败: "+err.Error())
		bot.Send(msg)
		return
	}
	updateNewInstance(newInstance)

	// 如果实例模板中没有指定可用性域，则使用第一个可用的域
	if instance.AvailabilityDomain == "" && len(availabilityDomains) > 0 {
		instance.AvailabilityDomain = *availabilityDomains[0].Name
	}

	messageText := fmt.Sprintf("确认创建以下配置的实例：\n\n"+
		"形状: %s\n"+
		"CPU: %g\n"+
		"内存: %g GB\n"+
		"操作系统: %s %s\n"+
		"引导卷大小: %d GB\n"+
		"可用性域: %s\n\n"+
		"是否确认创建？",
		instance.Shape, instance.Ocpus, instance.MemoryInGBs,
		instance.OperatingSystem, instance.OperatingSystemVersion,
		instance.BootVolumeSizeInGBs, instance.AvailabilityDomain)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("确认创建", "confirm_create_instance"),
			tgbotapi.NewInlineKeyboardButtonData("取消", "account_action:create_instance"),
		),
	)

	msg := tgbotapi.NewMessage(chatID, messageText)
	msg.ReplyMarkup = keyboard
	bot.Send(msg)
}

func startCreateInstance(chatID int64) {
	log.Printf("开始创建实例，chatID: %d", chatID)
	msg := tgbotapi.NewMessage(chatID, "正在创建实例，请稍候...")
	sentMsg, _ := bot.Send(msg)

	getInstanceCopy()

	sum, num := LaunchInstances(availabilityDomains)

	log.Printf("创建实例完成，总数: %d, 成功: %d", sum, num)
	resultMsg := fmt.Sprintf("创建实例结果：\n总数: %d\n成功: %d\n失败: %d", sum, num, sum-num)
	editMsg := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, resultMsg)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("返回实例列表", "account_action:list_instances"),
			tgbotapi.NewInlineKeyboardButtonData("返回主菜单", "select_account:"+strconv.Itoa(getCurrentAccountIndex())),
		),
	)
	editMsg.ReplyMarkup = &keyboard
	bot.Send(editMsg)
}
func sendMainMenu(chatID int64) {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("选择账户", "list_accounts"),
		),
	)

	msg := tgbotapi.NewMessage(chatID, "欢迎使用甲骨文实例管理工具，请选择操作：")
	msg.ReplyMarkup = keyboard

	bot.Send(msg)
}

func sendAccountList(chatID int64) {
	var keyboard [][]tgbotapi.InlineKeyboardButton

	for i, section := range oracleSections {
		button := tgbotapi.NewInlineKeyboardButtonData(section.Name(), fmt.Sprintf("select_account:%d", i))
		row := tgbotapi.NewInlineKeyboardRow(button)
		keyboard = append(keyboard, row)
	}

	keyboard = append(keyboard, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("返回主菜单", "main_menu"),
	))

	msg := tgbotapi.NewMessage(chatID, "请选择要操作的账户：")
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(keyboard...)

	bot.Send(msg)
}

func selectAccount(chatID int64, accountIndex int) {
	if accountIndex < 0 || accountIndex >= len(oracleSections) {
		msg := tgbotapi.NewMessage(chatID, "无效的账户选择")
		bot.Send(msg)
		return
	}

	oracleSection = oracleSections[accountIndex]
	err := initVar(oracleSection)
	if err != nil {
		msg := tgbotapi.NewMessage(chatID, "初始化账户失败："+err.Error())
		bot.Send(msg)
		return
	}

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("查看实例", "account_action:list_instances"),
			tgbotapi.NewInlineKeyboardButtonData("创建实例", "account_action:create_instance"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("管理引导卷", "account_action:manage_boot_volumes"),
			tgbotapi.NewInlineKeyboardButtonData("查看成本", "account_action:view_cost"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("返回主菜单", "main_menu"),
		),
	)

	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("已选择账户：%s\n请选择操作：", oracleSection.Name()))
	msg.ReplyMarkup = keyboard

	bot.Send(msg)
}

func handleAccountAction(chatID int64, action string) {
	switch action {
	case "list_instances":
		listInstancesTelegram(chatID)
	case "create_instance":
		createInstanceTelegram(chatID)
	case "manage_boot_volumes":
		manageBootVolumesTelegram(chatID)
	case "view_cost":
		viewCostTelegram(chatID)
	default:
		msg := tgbotapi.NewMessage(chatID, "未知操作")
		bot.Send(msg)
	}
}

func listInstancesTelegram(chatID int64) {
	msg := tgbotapi.NewMessage(chatID, "正在获取实例数据...")
	sentMsg, _ := bot.Send(msg)

	var instances []core.Instance
	var nextPage *string
	var err error
	for {
		var ins []core.Instance
		ins, nextPage, err = ListInstances(ctx, computeClient, nextPage)
		if err == nil {
			instances = append(instances, ins...)
		}
		if nextPage == nil || len(ins) == 0 {
			break
		}
	}

	if err != nil {
		editMsg := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, "获取实例失败: "+err.Error())
		bot.Send(editMsg)
		return
	}

	if len(instances) == 0 {
		editMsg := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, "没有找到任何实例")
		bot.Send(editMsg)
		return
	}

	var messageText strings.Builder
	messageText.WriteString("实例列表：\n\n")

	var keyboard [][]tgbotapi.InlineKeyboardButton

	for i, ins := range instances {
		messageText.WriteString(fmt.Sprintf("%d. %s (状态: %s)\n", i+1, *ins.DisplayName, getInstanceState(ins.LifecycleState)))
		button := tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("实例 %d", i+1), fmt.Sprintf("instance_details:%d", i))
		row := tgbotapi.NewInlineKeyboardRow(button)
		keyboard = append(keyboard, row)
	}

	keyboard = append(keyboard, tgbotapi.NewInlineKeyboardRow(
		tgbotapi.NewInlineKeyboardButtonData("返回", "select_account:"+strconv.Itoa(getCurrentAccountIndex())),
	))

	editMsg := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, messageText.String())
	editMsg.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: keyboard,
	}
	bot.Send(editMsg)
}

func getCurrentAccountIndex() int {
	for i, section := range oracleSections {
		if section == oracleSection {
			return i
		}
	}
	return -1
}

func initVar(oracleSec *ini.Section) (err error) {
	oracleSectionName = oracleSec.Name()
	oracle = Oracle{}
	err = oracleSec.MapTo(&oracle)
	if err != nil {
		printlnErr("解析账号相关参数失败", err.Error())
		return
	}
	provider, err = getProvider(oracle)
	if err != nil {
		printlnErr("获取 Provider 失败", err.Error())
		return
	}

	computeClient, err = core.NewComputeClientWithConfigurationProvider(provider)
	if err != nil {
		printlnErr("创建 ComputeClient 失败", err.Error())
		return
	}
	setProxyOrNot(&computeClient.BaseClient)
	networkClient, err = core.NewVirtualNetworkClientWithConfigurationProvider(provider)
	if err != nil {
		printlnErr("创建 VirtualNetworkClient 失败", err.Error())
		return
	}
	setProxyOrNot(&networkClient.BaseClient)
	storageClient, err = core.NewBlockstorageClientWithConfigurationProvider(provider)
	if err != nil {
		printlnErr("创建 BlockstorageClient 失败", err.Error())
		return
	}
	setProxyOrNot(&storageClient.BaseClient)
	identityClient, err = identity.NewIdentityClientWithConfigurationProvider(provider)
	if err != nil {
		printlnErr("创建 IdentityClient 失败", err.Error())
		return
	}
	setProxyOrNot(&identityClient.BaseClient)
	// 获取可用性域
	availabilityDomains, err = ListAvailabilityDomains()
	if err != nil {
		return fmt.Errorf("获取可用性域失败: %v", err)
	}
	return nil
}

// 返回值 sum: 创建实例总数; num: 创建成功的个数
func LaunchInstances(ads []identity.AvailabilityDomain) (sum, num int32) {
	/* 创建实例的几种情况
	 * 1. 设置了 availabilityDomain 参数，即在设置的可用性域中创建 sum 个实例。
	 * 2. 没有设置 availabilityDomain 但是设置了 each 参数。即在获取的每个可用性域中创建 each 个实例，创建的实例总数 sum =  each * adCount。
	 * 3. 没有设置 availabilityDomain 且没有设置 each 参数，即在获取到的可用性域中创建的实例总数为 sum。
	 */
	// 检查可用性域列表是否为空
	if len(ads) == 0 {
		log.Println("错误：可用性域列表为空")
		return 0, 0
	}

	//可用性域数量
	var adCount int32 = int32(len(ads))
	adName := common.String(instance.AvailabilityDomain)
	each := instance.Each
	sum = instance.Sum

	// 没有设置可用性域并且没有设置each时，才有用。
	var usableAds = make([]identity.AvailabilityDomain, 0)

	//可用性域不固定，即没有提供 availabilityDomain 参数
	var AD_NOT_FIXED bool = false
	var EACH_AD = false
	if adName == nil || *adName == "" {
		AD_NOT_FIXED = true
		if each > 0 {
			EACH_AD = true
			sum = each * adCount
		} else {
			EACH_AD = false
			usableAds = ads
		}
	}

	name := instance.InstanceDisplayName
	if name == "" {
		name = time.Now().Format("instance-20060102-1504")
	}
	displayName := common.String(name)
	if sum > 1 {
		displayName = common.String(name + "-1")
	}
	// create the launch instance request
	request := core.LaunchInstanceRequest{}
	request.CompartmentId = common.String(oracle.Tenancy)
	request.DisplayName = displayName

	// Get a image.
	fmt.Println("正在获取系统镜像...")
	image, err := GetImage(ctx, computeClient)
	if err != nil {
		printlnErr("获取系统镜像失败", err.Error())
		return
	}
	fmt.Println("系统镜像:", *image.DisplayName)

	var shape core.Shape
	if strings.Contains(strings.ToLower(instance.Shape), "flex") && instance.Ocpus > 0 && instance.MemoryInGBs > 0 {
		shape.Shape = &instance.Shape
		shape.Ocpus = &instance.Ocpus
		shape.MemoryInGBs = &instance.MemoryInGBs
	} else {
		fmt.Println("正在获取Shape信息...")
		shape, err = getShape(image.Id, instance.Shape)
		if err != nil {
			printlnErr("获取Shape信息失败", err.Error())
			return
		}
	}

	request.Shape = shape.Shape
	if strings.Contains(strings.ToLower(*shape.Shape), "flex") {
		request.ShapeConfig = &core.LaunchInstanceShapeConfigDetails{
			Ocpus:       shape.Ocpus,
			MemoryInGBs: shape.MemoryInGBs,
		}
		if instance.Burstable == "1/8" {
			request.ShapeConfig.BaselineOcpuUtilization = core.LaunchInstanceShapeConfigDetailsBaselineOcpuUtilization8
		} else if instance.Burstable == "1/2" {
			request.ShapeConfig.BaselineOcpuUtilization = core.LaunchInstanceShapeConfigDetailsBaselineOcpuUtilization2
		}
	}

	// create a subnet or get the one already created
	fmt.Println("正在获取子网...")
	subnet, err := CreateOrGetNetworkInfrastructure(ctx, networkClient)
	if err != nil {
		printlnErr("获取子网失败", err.Error())
		return
	}
	fmt.Println("子网:", *subnet.DisplayName)
	request.CreateVnicDetails = &core.CreateVnicDetails{SubnetId: subnet.Id}

	sd := core.InstanceSourceViaImageDetails{}
	sd.ImageId = image.Id
	if instance.BootVolumeSizeInGBs > 0 {
		sd.BootVolumeSizeInGBs = common.Int64(instance.BootVolumeSizeInGBs)
	}
	request.SourceDetails = sd
	request.IsPvEncryptionInTransitEnabled = common.Bool(true)

	metaData := map[string]string{}
	metaData["ssh_authorized_keys"] = instance.SSH_Public_Key
	if instance.CloudInit != "" {
		metaData["user_data"] = instance.CloudInit
	}
	request.Metadata = metaData

	minTime := instance.MinTime
	maxTime := instance.MaxTime

	SKIP_RETRY_MAP := make(map[int32]bool)
	var usableAdsTemp = make([]identity.AvailabilityDomain, 0)

	retry := instance.Retry // 重试次数
	var failTimes int32 = 0 // 失败次数

	// 记录尝试创建实例的次数
	var runTimes int32 = 0

	var adIndex int32 = 0 // 当前可用性域下标
	var pos int32 = 0     // for 循环次数
	var SUCCESS = false   // 创建是否成功

	var startTime = time.Now()

	var bootVolumeSize float64
	if instance.BootVolumeSizeInGBs > 0 {
		bootVolumeSize = float64(instance.BootVolumeSizeInGBs)
	} else {
		bootVolumeSize = math.Round(float64(*image.SizeInMBs) / float64(1024))
	}
	printf("\033[1;36m[%s] 开始创建 %s 实例, OCPU: %g 内存: %g 引导卷: %g \033[0m\n", oracleSectionName, *shape.Shape, *shape.Ocpus, *shape.MemoryInGBs, bootVolumeSize)
	if EACH {
		text := fmt.Sprintf("正在尝试创建第 %d 个实例...⏳\n区域: %s\n实例配置: %s\nOCPU计数: %g\n内存(GB): %g\n引导卷(GB): %g\n创建个数: %d", pos+1, oracle.Region, *shape.Shape, *shape.Ocpus, *shape.MemoryInGBs, bootVolumeSize, sum)
		_, err := sendMessage("", text)
		if err != nil {
			printlnErr("Telegram 消息提醒发送失败", err.Error())
		}
	}

	for pos < sum {

		if AD_NOT_FIXED {
			if EACH_AD {
				if pos%each == 0 && failTimes == 0 {
					adName = ads[adIndex].Name
					adIndex++
				}
			} else {
				if SUCCESS {
					adIndex = 0
				}
				if adIndex >= adCount {
					adIndex = 0
				}
				// 在使用 ads[adIndex] 之前，确保 adIndex 在有效范围内
				if adIndex < 0 || adIndex >= adCount {
					log.Printf("错误：无效的可用性域索引 %d", adIndex)
					return sum, num
				}
				//adName = ads[adIndex].Name
				adName = usableAds[adIndex].Name
				adIndex++
			}
		}

		runTimes++
		printf("\033[1;36m[%s] 正在尝试创建第 %d 个实例, AD: %s\033[0m\n", oracleSectionName, pos+1, *adName)
		printf("\033[1;36m[%s] 当前尝试次数: %d \033[0m\n", oracleSectionName, runTimes)
		request.AvailabilityDomain = adName
		createResp, err := computeClient.LaunchInstance(ctx, request)

		if err == nil {
			// 创建实例成功
			SUCCESS = true
			num++ //成功个数+1

			duration := fmtDuration(time.Since(startTime))

			printf("\033[1;32m[%s] 第 %d 个实例抢到了🎉, 正在启动中请稍等...⌛️ \033[0m\n", oracleSectionName, pos+1)
			var msg Message
			var msgErr error
			var text string
			if EACH {
				text = fmt.Sprintf("第 %d 个实例抢到了🎉, 正在启动中请稍等...⌛️\n区域: %s\n实例名称: %s\n公共IP: 获取中...⏳\n可用性域:%s\n实例配置: %s\nOCPU计数: %g\n内存(GB): %g\n引导卷(GB): %g\n创建个数: %d\n尝试次数: %d\n耗时: %s", pos+1, oracle.Region, *createResp.Instance.DisplayName, *createResp.Instance.AvailabilityDomain, *shape.Shape, *shape.Ocpus, *shape.MemoryInGBs, bootVolumeSize, sum, runTimes, duration)
				msg, msgErr = sendMessage("", text)
			}
			// 获取实例公共IP
			var strIps string
			ips, err := getInstancePublicIps(createResp.Instance.Id)
			if err != nil {
				printf("\033[1;32m[%s] 第 %d 个实例抢到了🎉, 但是启动失败❌ 错误信息: \033[0m%s\n", oracleSectionName, pos+1, err.Error())
				text = fmt.Sprintf("第 %d 个实例抢到了🎉, 但是启动失败❌实例已被终止😔\n区域: %s\n实例名称: %s\n可用性域:%s\n实例配置: %s\nOCPU计数: %g\n内存(GB): %g\n引导卷(GB): %g\n创建个数: %d\n尝试次数: %d\n耗时: %s", pos+1, oracle.Region, *createResp.Instance.DisplayName, *createResp.Instance.AvailabilityDomain, *shape.Shape, *shape.Ocpus, *shape.MemoryInGBs, bootVolumeSize, sum, runTimes, duration)
			} else {
				strIps = strings.Join(ips, ",")
				printf("\033[1;32m[%s] 第 %d 个实例抢到了🎉, 启动成功✅. 实例名称: %s, 公共IP: %s\033[0m\n", oracleSectionName, pos+1, *createResp.Instance.DisplayName, strIps)
				text = fmt.Sprintf("第 %d 个实例抢到了🎉, 启动成功✅\n区域: %s\n实例名称: %s\n公共IP: %s\n可用性域:%s\n实例配置: %s\nOCPU计数: %g\n内存(GB): %g\n引导卷(GB): %g\n创建个数: %d\n尝试次数: %d\n耗时: %s", pos+1, oracle.Region, *createResp.Instance.DisplayName, strIps, *createResp.Instance.AvailabilityDomain, *shape.Shape, *shape.Ocpus, *shape.MemoryInGBs, bootVolumeSize, sum, runTimes, duration)
			}
			if EACH {
				if msgErr != nil {
					sendMessage("", text)
				} else {
					editMessage(msg.MessageId, "", text)
				}
			}

			sleepRandomSecond(minTime, maxTime)

			displayName = common.String(fmt.Sprintf("%s-%d", name, pos+1))
			request.DisplayName = displayName

		} else {
			// 创建实例失败
			SUCCESS = false
			// 错误信息
			errInfo := err.Error()
			// 是否跳过重试
			SKIP_RETRY := false

			//isRetryable := common.IsErrorRetryableByDefault(err)
			//isNetErr := common.IsNetworkError(err)
			servErr, isServErr := common.IsServiceError(err)

			// API Errors: https://docs.cloud.oracle.com/Content/API/References/apierrors.htm

			if isServErr && (400 <= servErr.GetHTTPStatusCode() && servErr.GetHTTPStatusCode() <= 405) ||
				(servErr.GetHTTPStatusCode() == 409 && !strings.EqualFold(servErr.GetCode(), "IncorrectState")) ||
				servErr.GetHTTPStatusCode() == 412 || servErr.GetHTTPStatusCode() == 413 || servErr.GetHTTPStatusCode() == 422 ||
				servErr.GetHTTPStatusCode() == 431 || servErr.GetHTTPStatusCode() == 501 {
				// 不可重试
				if isServErr {
					errInfo = servErr.GetMessage()
				}
				duration := fmtDuration(time.Since(startTime))
				printf("\033[1;31m[%s] 第 %d 个实例创建失败了❌, 错误信息: \033[0m%s\n", oracleSectionName, pos+1, errInfo)
				if EACH {
					text := fmt.Sprintf("第 %d 个实例创建失败了❌\n错误信息: %s\n区域: %s\n可用性域: %s\n实例配置: %s\nOCPU计数: %g\n内存(GB): %g\n引导卷(GB): %g\n创建个数: %d\n尝试次数: %d\n耗时:%s", pos+1, errInfo, oracle.Region, *adName, *shape.Shape, *shape.Ocpus, *shape.MemoryInGBs, bootVolumeSize, sum, runTimes, duration)
					sendMessage("", text)
				}

				SKIP_RETRY = true
				if AD_NOT_FIXED && !EACH_AD {
					SKIP_RETRY_MAP[adIndex-1] = true
				}

			} else {
				// 可重试
				if isServErr {
					errInfo = servErr.GetMessage()
				}
				printf("\033[1;31m[%s] 创建失败, Error: \033[0m%s\n", oracleSectionName, errInfo)

				SKIP_RETRY = false
				if AD_NOT_FIXED && !EACH_AD {
					SKIP_RETRY_MAP[adIndex-1] = false
				}
			}

			sleepRandomSecond(minTime, maxTime)

			if AD_NOT_FIXED {
				if !EACH_AD {
					if adIndex < adCount {
						// 没有设置可用性域，且没有设置each。即在获取到的每个可用性域里尝试创建。当前使用的可用性域不是最后一个，继续尝试。
						continue
					} else {
						// 当前使用的可用性域是最后一个，判断失败次数是否达到重试次数，未达到重试次数继续尝试。
						failTimes++

						for index, skip := range SKIP_RETRY_MAP {
							if !skip {
								usableAdsTemp = append(usableAdsTemp, usableAds[index])
							}
						}

						// 重新设置 usableAds
						usableAds = usableAdsTemp
						adCount = int32(len(usableAds))

						// 重置变量
						usableAdsTemp = nil
						for k := range SKIP_RETRY_MAP {
							delete(SKIP_RETRY_MAP, k)
						}

						// 判断是否需要重试
						if (retry < 0 || failTimes <= retry) && adCount > 0 {
							continue
						}
					}

					adIndex = 0

				} else {
					// 没有设置可用性域，且设置了each，即在每个域创建each个实例。判断失败次数继续尝试。
					failTimes++
					if (retry < 0 || failTimes <= retry) && !SKIP_RETRY {
						continue
					}
				}

			} else {
				//设置了可用性域，判断是否需要重试
				failTimes++
				if (retry < 0 || failTimes <= retry) && !SKIP_RETRY {
					continue
				}
			}

		}

		// 重置变量
		usableAds = ads
		adCount = int32(len(usableAds))
		usableAdsTemp = nil
		for k := range SKIP_RETRY_MAP {
			delete(SKIP_RETRY_MAP, k)
		}

		// 成功或者失败次数达到重试次数，重置失败次数为0
		failTimes = 0

		// 重置尝试创建实例次数
		runTimes = 0
		startTime = time.Now()

		// for 循环次数+1
		pos++

		if pos < sum && EACH {
			text := fmt.Sprintf("正在尝试创建第 %d 个实例...⏳\n区域: %s\n实例配置: %s\nOCPU计数: %g\n内存(GB): %g\n引导卷(GB): %g\n创建个数: %d", pos+1, oracle.Region, *shape.Shape, *shape.Ocpus, *shape.MemoryInGBs, bootVolumeSize, sum)
			sendMessage("", text)
		}
	}
	return
}

func sleepRandomSecond(min, max int32) {
	var second int32
	if min <= 0 || max <= 0 {
		second = 1
	} else if min >= max {
		second = max
	} else {
		second = rand.Int31n(max-min) + min
	}
	printf("Sleep %d Second...\n", second)
	time.Sleep(time.Duration(second) * time.Second)
}

// ExampleLaunchInstance does create an instance
// NOTE: launch instance will create a new instance and VCN. please make sure delete the instance
// after execute this sample code, otherwise, you will be charged for the running instance

func getProvider(oracle Oracle) (common.ConfigurationProvider, error) {
	content, err := ioutil.ReadFile(oracle.Key_file)
	if err != nil {
		return nil, err
	}
	privateKey := string(content)
	privateKeyPassphrase := common.String(oracle.Key_password)
	return common.NewRawConfigurationProvider(oracle.Tenancy, oracle.User, oracle.Region, oracle.Fingerprint, privateKey, privateKeyPassphrase), nil
}

func currMouthFirstLastDay() (time.Time, time.Time) {
	// 获取当前时间
	now := time.Now().UTC()
	// 获取当前月份的第一天
	firstDay := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	// 获取下个月的第一天
	nextMonth := now.AddDate(0, 1, 0)
	firstDayOfNextMonth := time.Date(nextMonth.Year(), nextMonth.Month(), 1, 0, 0, 0, 0, nextMonth.Location())

	return firstDay, firstDayOfNextMonth
}

// 创建或获取基础网络设施
func CreateOrGetNetworkInfrastructure(ctx context.Context, c core.VirtualNetworkClient) (subnet core.Subnet, err error) {
	var vcn core.Vcn
	vcn, err = createOrGetVcn(ctx, c)
	if err != nil {
		return
	}
	var gateway core.InternetGateway
	gateway, err = createOrGetInternetGateway(c, vcn.Id)
	if err != nil {
		return
	}
	_, err = createOrGetRouteTable(c, gateway.Id, vcn.Id)
	if err != nil {
		return
	}
	subnet, err = createOrGetSubnetWithDetails(
		ctx, c, vcn.Id,
		common.String(instance.SubnetDisplayName),
		common.String("10.0.0.0/20"),
		common.String("subnetdns"),
		common.String(instance.AvailabilityDomain))
	return
}

// CreateOrGetSubnetWithDetails either creates a new Virtual Cloud Network (VCN) or get the one already exist
// with detail info
func createOrGetSubnetWithDetails(ctx context.Context, c core.VirtualNetworkClient, vcnID *string,
	displayName *string, cidrBlock *string, dnsLabel *string, availableDomain *string) (subnet core.Subnet, err error) {
	var subnets []core.Subnet
	subnets, err = listSubnets(ctx, c, vcnID)
	if err != nil {
		return
	}

	if displayName == nil {
		displayName = common.String(instance.SubnetDisplayName)
	}

	if len(subnets) > 0 && *displayName == "" {
		subnet = subnets[0]
		return
	}

	// check if the subnet has already been created
	for _, element := range subnets {
		if *element.DisplayName == *displayName {
			// find the subnet, return it
			subnet = element
			return
		}
	}

	// create a new subnet
	fmt.Printf("开始创建Subnet（没有可用的Subnet，或指定的Subnet不存在）\n")
	// 子网名称为空，以当前时间为名称创建子网
	if *displayName == "" {
		displayName = common.String(time.Now().Format("subnet-20060102-1504"))
	}
	request := core.CreateSubnetRequest{}
	//request.AvailabilityDomain = availableDomain //省略此属性创建区域性子网(regional subnet)，提供此属性创建特定于可用性域的子网。建议创建区域性子网。
	request.CompartmentId = &oracle.Tenancy
	request.CidrBlock = cidrBlock
	request.DisplayName = displayName
	request.DnsLabel = dnsLabel
	request.RequestMetadata = getCustomRequestMetadataWithRetryPolicy()

	request.VcnId = vcnID
	var r core.CreateSubnetResponse
	r, err = c.CreateSubnet(ctx, request)
	if err != nil {
		return
	}
	// retry condition check, stop unitl return true
	pollUntilAvailable := func(r common.OCIOperationResponse) bool {
		if converted, ok := r.Response.(core.GetSubnetResponse); ok {
			return converted.LifecycleState != core.SubnetLifecycleStateAvailable
		}
		return true
	}

	pollGetRequest := core.GetSubnetRequest{
		SubnetId:        r.Id,
		RequestMetadata: helpers.GetRequestMetadataWithCustomizedRetryPolicy(pollUntilAvailable),
	}

	// wait for lifecyle become running
	_, err = c.GetSubnet(ctx, pollGetRequest)
	if err != nil {
		return
	}

	// update the security rules
	getReq := core.GetSecurityListRequest{
		SecurityListId:  common.String(r.SecurityListIds[0]),
		RequestMetadata: getCustomRequestMetadataWithRetryPolicy(),
	}

	var getResp core.GetSecurityListResponse
	getResp, err = c.GetSecurityList(ctx, getReq)
	if err != nil {
		return
	}

	// this security rule allows remote control the instance
	/*portRange := core.PortRange{
		Max: common.Int(1521),
		Min: common.Int(1521),
	}*/

	newRules := append(getResp.IngressSecurityRules, core.IngressSecurityRule{
		//Protocol: common.String("6"), // TCP
		Protocol: common.String("all"), // 允许所有协议
		Source:   common.String("0.0.0.0/0"),
		/*TcpOptions: &core.TcpOptions{
			DestinationPortRange: &portRange, // 省略该参数，允许所有目标端口。
		},*/
	})

	updateReq := core.UpdateSecurityListRequest{
		SecurityListId:  common.String(r.SecurityListIds[0]),
		RequestMetadata: getCustomRequestMetadataWithRetryPolicy(),
	}

	updateReq.IngressSecurityRules = newRules

	_, err = c.UpdateSecurityList(ctx, updateReq)
	if err != nil {
		return
	}
	fmt.Printf("Subnet创建成功: %s\n", *r.Subnet.DisplayName)
	subnet = r.Subnet
	return
}

// 列出指定虚拟云网络 (VCN) 中的所有子网
func listSubnets(ctx context.Context, c core.VirtualNetworkClient, vcnID *string) (subnets []core.Subnet, err error) {
	request := core.ListSubnetsRequest{
		CompartmentId:   &oracle.Tenancy,
		VcnId:           vcnID,
		RequestMetadata: getCustomRequestMetadataWithRetryPolicy(),
	}
	var r core.ListSubnetsResponse
	r, err = c.ListSubnets(ctx, request)
	if err != nil {
		return
	}
	subnets = r.Items
	return
}

// 创建一个新的虚拟云网络 (VCN) 或获取已经存在的虚拟云网络
func createOrGetVcn(ctx context.Context, c core.VirtualNetworkClient) (core.Vcn, error) {
	var vcn core.Vcn
	vcnItems, err := listVcns(ctx, c)
	if err != nil {
		return vcn, err
	}
	displayName := common.String(instance.VcnDisplayName)
	if len(vcnItems) > 0 && *displayName == "" {
		vcn = vcnItems[0]
		return vcn, err
	}
	for _, element := range vcnItems {
		if *element.DisplayName == instance.VcnDisplayName {
			// VCN already created, return it
			vcn = element
			return vcn, err
		}
	}
	// create a new VCN
	fmt.Println("开始创建VCN（没有可用的VCN，或指定的VCN不存在）")
	if *displayName == "" {
		displayName = common.String(time.Now().Format("vcn-20060102-1504"))
	}
	request := core.CreateVcnRequest{}
	request.RequestMetadata = getCustomRequestMetadataWithRetryPolicy()
	request.CidrBlock = common.String("10.0.0.0/16")
	request.CompartmentId = common.String(oracle.Tenancy)
	request.DisplayName = displayName
	request.DnsLabel = common.String("vcndns")
	r, err := c.CreateVcn(ctx, request)
	if err != nil {
		return vcn, err
	}
	fmt.Printf("VCN创建成功: %s\n", *r.Vcn.DisplayName)
	vcn = r.Vcn
	return vcn, err
}

// 列出所有虚拟云网络 (VCN)
func listVcns(ctx context.Context, c core.VirtualNetworkClient) ([]core.Vcn, error) {
	request := core.ListVcnsRequest{
		CompartmentId:   &oracle.Tenancy,
		RequestMetadata: getCustomRequestMetadataWithRetryPolicy(),
	}
	r, err := c.ListVcns(ctx, request)
	if err != nil {
		return nil, err
	}
	return r.Items, err
}

// 创建或者获取 Internet 网关
func createOrGetInternetGateway(c core.VirtualNetworkClient, vcnID *string) (core.InternetGateway, error) {
	//List Gateways
	var gateway core.InternetGateway
	listGWRequest := core.ListInternetGatewaysRequest{
		CompartmentId:   &oracle.Tenancy,
		VcnId:           vcnID,
		RequestMetadata: getCustomRequestMetadataWithRetryPolicy(),
	}

	listGWRespone, err := c.ListInternetGateways(ctx, listGWRequest)
	if err != nil {
		fmt.Printf("Internet gateway list error: %s\n", err.Error())
		return gateway, err
	}

	if len(listGWRespone.Items) >= 1 {
		//Gateway with name already exists
		gateway = listGWRespone.Items[0]
	} else {
		//Create new Gateway
		fmt.Printf("开始创建Internet网关\n")
		enabled := true
		createGWDetails := core.CreateInternetGatewayDetails{
			CompartmentId: &oracle.Tenancy,
			IsEnabled:     &enabled,
			VcnId:         vcnID,
		}

		createGWRequest := core.CreateInternetGatewayRequest{
			CreateInternetGatewayDetails: createGWDetails,
			RequestMetadata:              getCustomRequestMetadataWithRetryPolicy()}

		createGWResponse, err := c.CreateInternetGateway(ctx, createGWRequest)

		if err != nil {
			fmt.Printf("Internet gateway create error: %s\n", err.Error())
			return gateway, err
		}
		gateway = createGWResponse.InternetGateway
		fmt.Printf("Internet网关创建成功: %s\n", *gateway.DisplayName)
	}
	return gateway, err
}

// 创建或者获取路由表
func createOrGetRouteTable(c core.VirtualNetworkClient, gatewayID, VcnID *string) (routeTable core.RouteTable, err error) {
	//List Route Table
	listRTRequest := core.ListRouteTablesRequest{
		CompartmentId:   &oracle.Tenancy,
		VcnId:           VcnID,
		RequestMetadata: getCustomRequestMetadataWithRetryPolicy(),
	}
	var listRTResponse core.ListRouteTablesResponse
	listRTResponse, err = c.ListRouteTables(ctx, listRTRequest)
	if err != nil {
		fmt.Printf("Route table list error: %s\n", err.Error())
		return
	}

	cidrRange := "0.0.0.0/0"
	rr := core.RouteRule{
		NetworkEntityId: gatewayID,
		Destination:     &cidrRange,
		DestinationType: core.RouteRuleDestinationTypeCidrBlock,
	}

	if len(listRTResponse.Items) >= 1 {
		//Default Route Table found and has at least 1 route rule
		if len(listRTResponse.Items[0].RouteRules) >= 1 {
			routeTable = listRTResponse.Items[0]
			//Default Route table needs route rule adding
		} else {
			fmt.Printf("路由表未添加规则，开始添加Internet路由规则\n")
			updateRTDetails := core.UpdateRouteTableDetails{
				RouteRules: []core.RouteRule{rr},
			}

			updateRTRequest := core.UpdateRouteTableRequest{
				RtId:                    listRTResponse.Items[0].Id,
				UpdateRouteTableDetails: updateRTDetails,
				RequestMetadata:         getCustomRequestMetadataWithRetryPolicy(),
			}
			var updateRTResponse core.UpdateRouteTableResponse
			updateRTResponse, err = c.UpdateRouteTable(ctx, updateRTRequest)
			if err != nil {
				fmt.Printf("Error updating route table: %s\n", err)
				return
			}
			fmt.Printf("Internet路由规则添加成功\n")
			routeTable = updateRTResponse.RouteTable
		}

	} else {
		//No default route table found
		fmt.Printf("Error could not find VCN default route table, VCN OCID: %s Could not find route table.\n", *VcnID)
	}
	return
}

// 获取符合条件系统镜像中的第一个
func GetImage(ctx context.Context, c core.ComputeClient) (image core.Image, err error) {
	var images []core.Image
	images, err = listImages(ctx, c)
	if err != nil {
		return
	}
	if len(images) > 0 {
		image = images[0]
	} else {
		err = fmt.Errorf("未找到[%s %s]的镜像, 或该镜像不支持[%s]", instance.OperatingSystem, instance.OperatingSystemVersion, instance.Shape)
	}
	return
}

// 列出所有符合条件的系统镜像
func listImages(ctx context.Context, c core.ComputeClient) ([]core.Image, error) {
	if instance.OperatingSystem == "" || instance.OperatingSystemVersion == "" {
		return nil, errors.New("操作系统类型和版本不能为空, 请检查配置文件")
	}
	request := core.ListImagesRequest{
		CompartmentId:          common.String(oracle.Tenancy),
		OperatingSystem:        common.String(instance.OperatingSystem),
		OperatingSystemVersion: common.String(instance.OperatingSystemVersion),
		Shape:                  common.String(instance.Shape),
		RequestMetadata:        getCustomRequestMetadataWithRetryPolicy(),
	}
	r, err := c.ListImages(ctx, request)
	return r.Items, err
}

func getShape(imageId *string, shapeName string) (core.Shape, error) {
	var shape core.Shape
	shapes, err := listShapes(ctx, computeClient, imageId)
	if err != nil {
		return shape, err
	}
	for _, s := range shapes {
		if strings.EqualFold(*s.Shape, shapeName) {
			shape = s
			return shape, nil
		}
	}
	err = errors.New("没有符合条件的Shape")
	return shape, err
}

// ListShapes Lists the shapes that can be used to launch an instance within the specified compartment.
func listShapes(ctx context.Context, c core.ComputeClient, imageID *string) ([]core.Shape, error) {
	request := core.ListShapesRequest{
		CompartmentId:   common.String(oracle.Tenancy),
		ImageId:         imageID,
		RequestMetadata: getCustomRequestMetadataWithRetryPolicy(),
	}
	r, err := c.ListShapes(ctx, request)
	if err == nil && (r.Items == nil || len(r.Items) == 0) {
		err = errors.New("没有符合条件的Shape")
	}
	return r.Items, err
}

// 列出符合条件的可用性域
func ListAvailabilityDomains() ([]identity.AvailabilityDomain, error) {
	req := identity.ListAvailabilityDomainsRequest{
		CompartmentId:   common.String(oracle.Tenancy),
		RequestMetadata: getCustomRequestMetadataWithRetryPolicy(),
	}
	resp, err := identityClient.ListAvailabilityDomains(ctx, req)
	return resp.Items, err
}

func ListInstances(ctx context.Context, c core.ComputeClient, page *string) ([]core.Instance, *string, error) {
	req := core.ListInstancesRequest{
		CompartmentId:   common.String(oracle.Tenancy),
		RequestMetadata: getCustomRequestMetadataWithRetryPolicy(),
		Limit:           common.Int(100),
		Page:            page,
	}
	resp, err := c.ListInstances(ctx, req)
	return resp.Items, resp.OpcNextPage, err
}

func ListVnicAttachments(ctx context.Context, c core.ComputeClient, instanceId *string, page *string) ([]core.VnicAttachment, *string, error) {
	req := core.ListVnicAttachmentsRequest{
		CompartmentId:   common.String(oracle.Tenancy),
		RequestMetadata: getCustomRequestMetadataWithRetryPolicy(),
		Limit:           common.Int(100),
		Page:            page,
	}
	if instanceId != nil && *instanceId != "" {
		req.InstanceId = instanceId
	}
	resp, err := c.ListVnicAttachments(ctx, req)
	return resp.Items, resp.OpcNextPage, err
}

func GetVnic(ctx context.Context, c core.VirtualNetworkClient, vnicID *string) (core.Vnic, error) {
	req := core.GetVnicRequest{
		VnicId:          vnicID,
		RequestMetadata: getCustomRequestMetadataWithRetryPolicy(),
	}
	resp, err := c.GetVnic(ctx, req)
	if err != nil && resp.RawResponse != nil {
		err = errors.New(resp.RawResponse.Status)
	}
	return resp.Vnic, err
}

// 终止实例
// https://docs.oracle.com/en-us/iaas/api/#/en/iaas/20160918/Instance/TerminateInstance
func terminateInstance(id *string) error {
	request := core.TerminateInstanceRequest{
		InstanceId:         id,
		PreserveBootVolume: common.Bool(false),
		RequestMetadata:    getCustomRequestMetadataWithRetryPolicy(),
	}
	_, err := computeClient.TerminateInstance(ctx, request)
	return err

	//fmt.Println("terminating instance")

	/*
		// should retry condition check which returns a bool value indicating whether to do retry or not
		// it checks the lifecycle status equals to Terminated or not for this case
		shouldRetryFunc := func(r common.OCIOperationResponse) bool {
			if converted, ok := r.Response.(core.GetInstanceResponse); ok {
				return converted.LifecycleState != core.InstanceLifecycleStateTerminated
			}
			return true
		}

		pollGetRequest := core.GetInstanceRequest{
			InstanceId:      id,
			RequestMetadata: helpers.GetRequestMetadataWithCustomizedRetryPolicy(shouldRetryFunc),
		}

		_, pollErr := c.GetInstance(ctx, pollGetRequest)
		helpers.FatalIfError(pollErr)
		fmt.Println("instance terminated")
	*/
}

// 删除虚拟云网络
func deleteVcn(ctx context.Context, c core.VirtualNetworkClient, id *string) {
	request := core.DeleteVcnRequest{
		VcnId:           id,
		RequestMetadata: helpers.GetRequestMetadataWithDefaultRetryPolicy(),
	}

	fmt.Println("deleteing VCN")
	_, err := c.DeleteVcn(ctx, request)
	helpers.FatalIfError(err)

	// should retry condition check which returns a bool value indicating whether to do retry or not
	// it checks the lifecycle status equals to Terminated or not for this case
	shouldRetryFunc := func(r common.OCIOperationResponse) bool {
		if serviceError, ok := common.IsServiceError(r.Error); ok && serviceError.GetHTTPStatusCode() == 404 {
			// resource been deleted, stop retry
			return false
		}

		if converted, ok := r.Response.(core.GetVcnResponse); ok {
			return converted.LifecycleState != core.VcnLifecycleStateTerminated
		}
		return true
	}

	pollGetRequest := core.GetVcnRequest{
		VcnId:           id,
		RequestMetadata: helpers.GetRequestMetadataWithCustomizedRetryPolicy(shouldRetryFunc),
	}

	_, pollErr := c.GetVcn(ctx, pollGetRequest)
	if serviceError, ok := common.IsServiceError(pollErr); !ok ||
		(ok && serviceError.GetHTTPStatusCode() != 404) {
		// fail if the error is not service error or
		// if the error is service error and status code not equals to 404
		helpers.FatalIfError(pollErr)
	}
	fmt.Println("VCN deleted")
}

// 删除子网
func deleteSubnet(ctx context.Context, c core.VirtualNetworkClient, id *string) {
	request := core.DeleteSubnetRequest{
		SubnetId:        id,
		RequestMetadata: helpers.GetRequestMetadataWithDefaultRetryPolicy(),
	}

	_, err := c.DeleteSubnet(context.Background(), request)
	helpers.FatalIfError(err)

	fmt.Println("deleteing subnet")

	// should retry condition check which returns a bool value indicating whether to do retry or not
	// it checks the lifecycle status equals to Terminated or not for this case
	shouldRetryFunc := func(r common.OCIOperationResponse) bool {
		if serviceError, ok := common.IsServiceError(r.Error); ok && serviceError.GetHTTPStatusCode() == 404 {
			// resource been deleted
			return false
		}

		if converted, ok := r.Response.(core.GetSubnetResponse); ok {
			return converted.LifecycleState != core.SubnetLifecycleStateTerminated
		}
		return true
	}

	pollGetRequest := core.GetSubnetRequest{
		SubnetId:        id,
		RequestMetadata: helpers.GetRequestMetadataWithCustomizedRetryPolicy(shouldRetryFunc),
	}

	_, pollErr := c.GetSubnet(ctx, pollGetRequest)
	if serviceError, ok := common.IsServiceError(pollErr); !ok ||
		(ok && serviceError.GetHTTPStatusCode() != 404) {
		// fail if the error is not service error or
		// if the error is service error and status code not equals to 404
		helpers.FatalIfError(pollErr)
	}

	fmt.Println("subnet deleted")
}

func getInstance(instanceId *string) (core.Instance, error) {
	req := core.GetInstanceRequest{
		InstanceId:      instanceId,
		RequestMetadata: getCustomRequestMetadataWithRetryPolicy(),
	}
	resp, err := computeClient.GetInstance(ctx, req)
	return resp.Instance, err
}

func updateInstance(instanceId *string, displayName *string, ocpus, memoryInGBs *float32,
	details []core.InstanceAgentPluginConfigDetails, disable *bool) (core.UpdateInstanceResponse, error) {
	updateInstanceDetails := core.UpdateInstanceDetails{}
	if displayName != nil && *displayName != "" {
		updateInstanceDetails.DisplayName = displayName
	}
	shapeConfig := core.UpdateInstanceShapeConfigDetails{}
	if ocpus != nil && *ocpus > 0 {
		shapeConfig.Ocpus = ocpus
	}
	if memoryInGBs != nil && *memoryInGBs > 0 {
		shapeConfig.MemoryInGBs = memoryInGBs
	}
	updateInstanceDetails.ShapeConfig = &shapeConfig

	// Oracle Cloud Agent 配置
	if disable != nil && details != nil {
		for i := 0; i < len(details); i++ {
			if *disable {
				details[i].DesiredState = core.InstanceAgentPluginConfigDetailsDesiredStateDisabled
			} else {
				details[i].DesiredState = core.InstanceAgentPluginConfigDetailsDesiredStateEnabled
			}
		}
		agentConfig := core.UpdateInstanceAgentConfigDetails{
			IsMonitoringDisabled:  disable, // 是否禁用监控插件
			IsManagementDisabled:  disable, // 是否禁用管理插件
			AreAllPluginsDisabled: disable, // 是否禁用所有可用的插件（管理和监控插件）
			PluginsConfig:         details,
		}
		updateInstanceDetails.AgentConfig = &agentConfig
	}

	req := core.UpdateInstanceRequest{
		InstanceId:            instanceId,
		UpdateInstanceDetails: updateInstanceDetails,
		RequestMetadata:       getCustomRequestMetadataWithRetryPolicy(),
	}
	return computeClient.UpdateInstance(ctx, req)
}

func instanceAction(instanceId *string, action core.InstanceActionActionEnum) (ins core.Instance, err error) {
	req := core.InstanceActionRequest{
		InstanceId:      instanceId,
		Action:          action,
		RequestMetadata: getCustomRequestMetadataWithRetryPolicy(),
	}
	resp, err := computeClient.InstanceAction(ctx, req)
	ins = resp.Instance
	return
}

func changePublicIp(vnics []core.Vnic) (publicIp core.PublicIp, err error) {
	var vnic core.Vnic
	for _, v := range vnics {
		if *v.IsPrimary {
			vnic = v
		}
	}
	fmt.Println("正在获取私有IP...")
	var privateIps []core.PrivateIp
	privateIps, err = getPrivateIps(vnic.Id)
	if err != nil {
		printlnErr("获取私有IP失败", err.Error())
		return
	}
	var privateIp core.PrivateIp
	for _, p := range privateIps {
		if *p.IsPrimary {
			privateIp = p
		}
	}

	fmt.Println("正在获取公共IP OCID...")
	publicIp, err = getPublicIp(privateIp.Id)
	if err != nil {
		printlnErr("获取公共IP OCID 失败", err.Error())
	}
	fmt.Println("正在删除公共IP...")
	_, err = deletePublicIp(publicIp.Id)
	if err != nil {
		printlnErr("删除公共IP 失败", err.Error())
	}
	time.Sleep(3 * time.Second)
	fmt.Println("正在创建公共IP...")
	publicIp, err = createPublicIp(privateIp.Id)
	return
}

func getInstanceVnics(instanceId *string) (vnics []core.Vnic, err error) {
	vnicAttachments, _, err := ListVnicAttachments(ctx, computeClient, instanceId, nil)
	if err != nil {
		return
	}
	for _, vnicAttachment := range vnicAttachments {
		vnic, vnicErr := GetVnic(ctx, networkClient, vnicAttachment.VnicId)
		if vnicErr != nil {
			fmt.Printf("GetVnic error: %s\n", vnicErr.Error())
			continue
		}
		vnics = append(vnics, vnic)
	}
	return
}

// 更新指定的VNIC
func updateVnic(vnicId *string) (core.Vnic, error) {
	req := core.UpdateVnicRequest{
		VnicId:            vnicId,
		UpdateVnicDetails: core.UpdateVnicDetails{SkipSourceDestCheck: common.Bool(true)},
		RequestMetadata:   getCustomRequestMetadataWithRetryPolicy(),
	}
	resp, err := networkClient.UpdateVnic(ctx, req)
	return resp.Vnic, err
}

// 获取指定VNIC的私有IP
func getPrivateIps(vnicId *string) ([]core.PrivateIp, error) {
	req := core.ListPrivateIpsRequest{
		VnicId:          vnicId,
		RequestMetadata: getCustomRequestMetadataWithRetryPolicy(),
	}
	resp, err := networkClient.ListPrivateIps(ctx, req)
	if err == nil && (resp.Items == nil || len(resp.Items) == 0) {
		err = errors.New("私有IP为空")
	}
	return resp.Items, err
}

// 获取分配给指定私有IP的公共IP
func getPublicIp(privateIpId *string) (core.PublicIp, error) {
	req := core.GetPublicIpByPrivateIpIdRequest{
		GetPublicIpByPrivateIpIdDetails: core.GetPublicIpByPrivateIpIdDetails{PrivateIpId: privateIpId},
		RequestMetadata:                 getCustomRequestMetadataWithRetryPolicy(),
	}
	resp, err := networkClient.GetPublicIpByPrivateIpId(ctx, req)
	if err == nil && resp.PublicIp.Id == nil {
		err = errors.New("未分配公共IP")
	}
	return resp.PublicIp, err
}

// 删除公共IP
// 取消分配并删除指定公共IP（临时或保留）
// 如果仅需要取消分配保留的公共IP并将保留的公共IP返回到保留公共IP池，请使用updatePublicIp方法。
func deletePublicIp(publicIpId *string) (core.DeletePublicIpResponse, error) {
	req := core.DeletePublicIpRequest{
		PublicIpId:      publicIpId,
		RequestMetadata: getCustomRequestMetadataWithRetryPolicy()}
	return networkClient.DeletePublicIp(ctx, req)
}

// 创建公共IP
// 通过Lifetime指定创建临时公共IP还是保留公共IP。
// 创建临时公共IP，必须指定privateIpId，将临时公共IP分配给指定私有IP。
// 创建保留公共IP，可以不指定privateIpId。稍后可以使用updatePublicIp方法分配给私有IP。
func createPublicIp(privateIpId *string) (core.PublicIp, error) {
	var publicIp core.PublicIp
	req := core.CreatePublicIpRequest{
		CreatePublicIpDetails: core.CreatePublicIpDetails{
			CompartmentId: common.String(oracle.Tenancy),
			Lifetime:      core.CreatePublicIpDetailsLifetimeEphemeral,
			PrivateIpId:   privateIpId,
		},
		RequestMetadata: getCustomRequestMetadataWithRetryPolicy(),
	}
	resp, err := networkClient.CreatePublicIp(ctx, req)
	publicIp = resp.PublicIp
	return publicIp, err
}

// 更新保留公共IP
// 1. 将保留的公共IP分配给指定的私有IP。如果该公共IP已经分配给私有IP，会取消分配，然后重新分配给指定的私有IP。
// 2. PrivateIpId设置为空字符串，公共IP取消分配到关联的私有IP。
func updatePublicIp(publicIpId *string, privateIpId *string) (core.PublicIp, error) {
	req := core.UpdatePublicIpRequest{
		PublicIpId: publicIpId,
		UpdatePublicIpDetails: core.UpdatePublicIpDetails{
			PrivateIpId: privateIpId,
		},
		RequestMetadata: getCustomRequestMetadataWithRetryPolicy(),
	}
	resp, err := networkClient.UpdatePublicIp(ctx, req)
	return resp.PublicIp, err
}

// 根据实例OCID获取公共IP
func getInstancePublicIps(instanceId *string) (ips []string, err error) {
	// 多次尝试，避免刚抢购到实例，实例正在预配获取不到公共IP。
	var ins core.Instance
	for i := 0; i < 100; i++ {
		if ins.LifecycleState != core.InstanceLifecycleStateRunning {
			ins, err = getInstance(instanceId)
			if err != nil {
				continue
			}
			if ins.LifecycleState == core.InstanceLifecycleStateTerminating || ins.LifecycleState == core.InstanceLifecycleStateTerminated {
				err = errors.New("实例已终止😔")
				return
			}
			// if ins.LifecycleState != core.InstanceLifecycleStateRunning {
			// 	continue
			// }
		}

		var vnicAttachments []core.VnicAttachment
		vnicAttachments, _, err = ListVnicAttachments(ctx, computeClient, instanceId, nil)
		if err != nil {
			continue
		}
		if len(vnicAttachments) > 0 {
			for _, vnicAttachment := range vnicAttachments {
				vnic, vnicErr := GetVnic(ctx, networkClient, vnicAttachment.VnicId)
				if vnicErr != nil {
					printf("GetVnic error: %s\n", vnicErr.Error())
					continue
				}
				if vnic.PublicIp != nil && *vnic.PublicIp != "" {
					ips = append(ips, *vnic.PublicIp)
				}
			}
			return
		}
		time.Sleep(3 * time.Second)
	}
	return
}

// 列出引导卷
func getBootVolumes(availabilityDomain *string) ([]core.BootVolume, error) {
	req := core.ListBootVolumesRequest{
		AvailabilityDomain: availabilityDomain,
		CompartmentId:      common.String(oracle.Tenancy),
		RequestMetadata:    getCustomRequestMetadataWithRetryPolicy(),
	}
	resp, err := storageClient.ListBootVolumes(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("获取引导卷列表失败: %v", err)
	}
	return resp.Items, nil
}

// 获取指定引导卷
func getBootVolume(bootVolumeId *string) (core.BootVolume, error) {
	req := core.GetBootVolumeRequest{
		BootVolumeId:    bootVolumeId,
		RequestMetadata: getCustomRequestMetadataWithRetryPolicy(),
	}
	resp, err := storageClient.GetBootVolume(ctx, req)
	return resp.BootVolume, err
}

// 更新引导卷
func updateBootVolume(bootVolumeId *string, sizeInGBs *int64, vpusPerGB *int64) (core.BootVolume, error) {
	updateBootVolumeDetails := core.UpdateBootVolumeDetails{}
	if sizeInGBs != nil {
		updateBootVolumeDetails.SizeInGBs = sizeInGBs
	}
	if vpusPerGB != nil {
		updateBootVolumeDetails.VpusPerGB = vpusPerGB
	}
	req := core.UpdateBootVolumeRequest{
		BootVolumeId:            bootVolumeId,
		UpdateBootVolumeDetails: updateBootVolumeDetails,
		RequestMetadata:         getCustomRequestMetadataWithRetryPolicy(),
	}
	resp, err := storageClient.UpdateBootVolume(ctx, req)
	return resp.BootVolume, err
}

// 删除引导卷
func deleteBootVolume(bootVolumeId *string) (*http.Response, error) {
	req := core.DeleteBootVolumeRequest{
		BootVolumeId:    bootVolumeId,
		RequestMetadata: getCustomRequestMetadataWithRetryPolicy(),
	}
	resp, err := storageClient.DeleteBootVolume(ctx, req)
	return resp.RawResponse, err
}

// 分离引导卷
func detachBootVolume(bootVolumeAttachmentId *string) (*http.Response, error) {
	req := core.DetachBootVolumeRequest{
		BootVolumeAttachmentId: bootVolumeAttachmentId,
		RequestMetadata:        getCustomRequestMetadataWithRetryPolicy(),
	}
	resp, err := computeClient.DetachBootVolume(ctx, req)
	return resp.RawResponse, err
}

// 获取引导卷附件
func listBootVolumeAttachments(availabilityDomain, compartmentId, bootVolumeId *string) ([]core.BootVolumeAttachment, error) {
	req := core.ListBootVolumeAttachmentsRequest{
		AvailabilityDomain: availabilityDomain,
		CompartmentId:      compartmentId,
		BootVolumeId:       bootVolumeId,
		RequestMetadata:    getCustomRequestMetadataWithRetryPolicy(),
	}
	resp, err := computeClient.ListBootVolumeAttachments(ctx, req)
	return resp.Items, err
}

func sendMessage(name, text string) (msg Message, err error) {
	if token != "" && chat_id != "" {
		data := url.Values{
			"parse_mode": {"Markdown"},
			"chat_id":    {chat_id},
			"text":       {"🔰*甲骨文通知* " + name + "\n" + text},
		}
		var req *http.Request
		req, err = http.NewRequest(http.MethodPost, sendMessageUrl, strings.NewReader(data.Encode()))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		client := common.BaseClient{HTTPClient: &http.Client{}}
		setProxyOrNot(&client)
		var resp *http.Response
		resp, err = client.HTTPClient.Do(req)
		if err != nil {
			return
		}
		var body []byte
		body, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			return
		}
		err = json.Unmarshal(body, &msg)
		if err != nil {
			return
		}
		if !msg.OK {
			err = errors.New(msg.Description)
			return
		}
	}
	return
}

func editMessage(messageId int, name, text string) (msg Message, err error) {
	if token != "" && chat_id != "" {
		data := url.Values{
			"parse_mode": {"Markdown"},
			"chat_id":    {chat_id},
			"message_id": {strconv.Itoa(messageId)},
			"text":       {"🔰*甲骨文通知* " + name + "\n" + text},
		}
		var req *http.Request
		req, err = http.NewRequest(http.MethodPost, editMessageUrl, strings.NewReader(data.Encode()))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		client := common.BaseClient{HTTPClient: &http.Client{}}
		setProxyOrNot(&client)
		var resp *http.Response
		resp, err = client.HTTPClient.Do(req)
		if err != nil {
			return
		}
		var body []byte
		body, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			return
		}
		err = json.Unmarshal(body, &msg)
		if err != nil {
			return
		}
		if !msg.OK {
			err = errors.New(msg.Description)
			return
		}

	}
	return
}

func setProxyOrNot(client *common.BaseClient) {
	if proxy != "" {
		proxyURL, err := url.Parse(proxy)
		if err != nil {
			printlnErr("URL parse failed", err.Error())
			return
		}
		client.HTTPClient = &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
			},
		}
	}
}

func getInstanceState(state core.InstanceLifecycleStateEnum) string {
	var friendlyState string
	switch state {
	case core.InstanceLifecycleStateMoving:
		friendlyState = "正在移动"
	case core.InstanceLifecycleStateProvisioning:
		friendlyState = "正在预配"
	case core.InstanceLifecycleStateRunning:
		friendlyState = "正在运行"
	case core.InstanceLifecycleStateStarting:
		friendlyState = "正在启动"
	case core.InstanceLifecycleStateStopping:
		friendlyState = "正在停止"
	case core.InstanceLifecycleStateStopped:
		friendlyState = "已停止　"
	case core.InstanceLifecycleStateTerminating:
		friendlyState = "正在终止"
	case core.InstanceLifecycleStateTerminated:
		friendlyState = "已终止　"
	default:
		friendlyState = string(state)
	}
	return friendlyState
}

func getBootVolumeState(state core.BootVolumeLifecycleStateEnum) string {
	var friendlyState string
	switch state {
	case core.BootVolumeLifecycleStateProvisioning:
		friendlyState = "正在预配"
	case core.BootVolumeLifecycleStateRestoring:
		friendlyState = "正在恢复"
	case core.BootVolumeLifecycleStateAvailable:
		friendlyState = "可用　　"
	case core.BootVolumeLifecycleStateTerminating:
		friendlyState = "正在终止"
	case core.BootVolumeLifecycleStateTerminated:
		friendlyState = "已终止　"
	case core.BootVolumeLifecycleStateFaulty:
		friendlyState = "故障　　"
	default:
		friendlyState = string(state)
	}
	return friendlyState
}

func fmtDuration(d time.Duration) string {
	if d.Seconds() < 1 {
		return "< 1 秒"
	}
	var buffer bytes.Buffer
	//days := int(d.Hours() / 24)
	//hours := int(math.Mod(d.Hours(), 24))
	//minutes := int(math.Mod(d.Minutes(), 60))
	//seconds := int(math.Mod(d.Seconds(), 60))

	days := int(d / (time.Hour * 24))
	hours := int((d % (time.Hour * 24)).Hours())
	minutes := int((d % time.Hour).Minutes())
	seconds := int((d % time.Minute).Seconds())

	if days > 0 {
		buffer.WriteString(fmt.Sprintf("%d 天 ", days))
	}
	if hours > 0 {
		buffer.WriteString(fmt.Sprintf("%d 时 ", hours))
	}
	if minutes > 0 {
		buffer.WriteString(fmt.Sprintf("%d 分 ", minutes))
	}
	if seconds > 0 {
		buffer.WriteString(fmt.Sprintf("%d 秒", seconds))
	}
	return buffer.String()
}

func printf(format string, a ...interface{}) {
	fmt.Printf("%s ", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Printf(format, a...)
}

func printlnErr(desc, detail string) {
	fmt.Printf("\033[1;31mError: %s. %s\033[0m\n", desc, detail)
}

func getCustomRequestMetadataWithRetryPolicy() common.RequestMetadata {
	return common.RequestMetadata{
		RetryPolicy: getCustomRetryPolicy(),
	}
}

func getCustomRetryPolicy() *common.RetryPolicy {
	// how many times to do the retry
	attempts := uint(3)
	// retry for all non-200 status code
	retryOnAllNon200ResponseCodes := func(r common.OCIOperationResponse) bool {
		return !(r.Error == nil && 199 < r.Response.HTTPResponse().StatusCode && r.Response.HTTPResponse().StatusCode < 300)
	}
	policy := common.NewRetryPolicyWithOptions(
		// only base off DefaultRetryPolicyWithoutEventualConsistency() if we're not handling eventual consistency
		common.WithConditionalOption(!false, common.ReplaceWithValuesFromRetryPolicy(common.DefaultRetryPolicyWithoutEventualConsistency())),
		common.WithMaximumNumberAttempts(attempts),
		common.WithShouldRetryOperation(retryOnAllNon200ResponseCodes))
	return &policy
}
