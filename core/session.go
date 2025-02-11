// Copyright © 2022 zc2638 <zc2638@qq.com>.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package core

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zc2638/ddshop/asserts"
	"github.com/zc2638/ddshop/pkg/notice"

	"golang.org/x/sync/errgroup"

	"github.com/AlecAivazis/survey/v2"
	"github.com/go-resty/resty/v2"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

func NewSession(cfg *Config) (*Session, error) {
	for k, v := range cfg.Periods {
		start, err := time.Parse("15:04", v.Start)
		if err != nil {
			return nil, fmt.Errorf("解析时间段 %d 开始时间(%s)失败: %v", k, v.Start, err)
		}
		end, err := time.Parse("15:04", v.End)
		if err != nil {
			return nil, fmt.Errorf("解析时间段 %d 结束时间(%s)失败: %v", k, v.Start, err)
		}
		cfg.Periods[k].startHour = start.Hour()
		cfg.Periods[k].startMinute = start.Minute()
		cfg.Periods[k].endHour = end.Hour()
		cfg.Periods[k].endMinute = end.Minute()
	}

	cookie := cfg.Cookie
	if !strings.HasPrefix(cookie, "DDXQSESSID=") {
		cookie = "DDXQSESSID=" + cookie
	}

	header := make(http.Header)
	header.Set("Host", "maicai.api.ddxq.mobi")
	header.Set("user-agent", "Mozilla/5.0 (Linux; Android 9; LIO-AN00 Build/LIO-AN00; wv) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/92.0.4515.131 Mobile Safari/537.36 xzone/9.47.0 station_id/null")
	header.Set("accept", "application/json, text/plain, */*")
	header.Set("content-type", "application/x-www-form-urlencoded")
	header.Set("origin", "https://wx.m.ddxq.mobi")
	header.Set("x-requested-with", "com.yaya.zone")
	header.Set("sec-fetch-site", "same-site")
	header.Set("sec-fetch-mode", "cors")
	header.Set("sec-fetch-dest", "empty")
	header.Set("referer", "https://wx.m.ddxq.mobi/")
	header.Set("accept-language", "zh-CN,zh;q=0.9,en-US;q=0.8,en;q=0.7")
	header.Set("cookie", cookie)

	client := resty.New()
	client.Header = header
	return &Session{
		cfg:       cfg,
		client:    client,
		successCh: make(chan struct{}, 1),
		stopCh:    make(chan struct{}, 1),

		apiVersion:  "9.49.2",
		appVersion:  "2.82.0",
		channel:     "applet",
		appClientID: "4",
	}, nil
}

type Session struct {
	cfg       *Config
	client    *resty.Client
	successCh chan struct{}
	stopCh    chan struct{}

	channel     string
	apiVersion  string
	appVersion  string
	appClientID string

	UserID  string
	PayType int64
	Address *AddressItem
	Reserve ReserveTime
}

func (s *Session) Start() error {
	if len(s.cfg.Periods) == 0 {
		return s.start()
	}

	for {
		second := time.Now().Second()
		if second == 0 {
			break
		}
		sleepInterval := 60 - second
		logrus.Warningf("当前秒数不为 0，需等待 %d 秒后开启自动助手", sleepInterval)
		time.Sleep(time.Duration(sleepInterval) * time.Second)
	}

	currentStartHour, currentStartMinute := -1, -1

	ticker := time.NewTicker(time.Minute)
	for {
		logrus.Warningf("开始任务侦查")
		now := time.Now()
		hour := now.Hour()
		minute := now.Minute()

		for _, v := range s.cfg.Periods {
			if currentStartHour > -1 && (currentStartHour != v.startHour || currentStartMinute != v.startMinute) {
				continue
			}

			start := false
			end := false
			if v.startHour > hour {
				start = true
			}
			if v.startHour == hour && v.startMinute <= minute {
				start = true
			}
			if v.endHour < hour {
				end = true
			}
			if v.endHour == hour && v.endMinute <= minute {
				end = true
			}

			if start && !end && currentStartHour != v.startHour {
				if len(s.stopCh) > 0 {
					s.stopCh = make(chan struct{}, 1)
				}
				go s.start()
				currentStartHour = v.startHour
				currentStartMinute = v.startMinute
				break
			}
			if start && end {
				if len(s.stopCh) == 0 {
					s.stopCh <- struct{}{}
				}
			}
		}
		<-ticker.C
	}
}

func (s *Session) start() error {
	if err := s.GetUser(); err != nil {
		return fmt.Errorf("获取用户信息失败: %v", err)
	}
	if err := s.Choose(); err != nil {
		return err
	}
	fmt.Println()

	ctx, cancelFunc := context.WithCancel(context.Background())
	go func() {
		for {
			select {
			case <-ctx.Done():
				logrus.Warningf("context done")
				return
			default:
			}
			if err := s.run(); err != nil {
				switch err {
				case ErrorNoValidProduct:
					sleepInterval := 30
					logrus.Errorf("购物车中无有效商品，请先前往app添加或勾选，%d 秒后重试！", sleepInterval)
					time.Sleep(time.Duration(sleepInterval) * time.Second)
				case ErrorNoReserveTime:
					sleepInterval := 3 + rand.Intn(6)
					logrus.Warningf("暂无可预约的时间，%d 秒后重试！", sleepInterval)
					time.Sleep(time.Duration(sleepInterval) * time.Second)
				default:
					logrus.Error(err)
				}
				fmt.Println()
			}
		}
	}()
	select {
	case <-s.stopCh:
		cancelFunc()
		logrus.Error("当前时间段内未抢到，等待下个时间段")
		return ErrorOutPeriod
	case <-s.successCh:
		cancelFunc()
		LoopRun(10, func() {
			logrus.Info("抢菜成功，请尽快支付!")
		})

		go func() {
			if s.cfg.BarkKey == "" {
				return
			}
			ins := notice.NewBark(s.cfg.BarkKey)
			if err := ins.Send("抢菜成功", "叮咚买菜 抢菜成功，请尽快支付！"); err != nil {
				logrus.Warningf("Bark消息通知失败: %v", err)
			}
		}()

		if err := asserts.Play(); err != nil {
			logrus.Warningf("播放成功提示音乐失败: %v", err)
		}
		// 异步放歌，歌曲有3分钟
		time.Sleep(3 * time.Minute)
		return nil
	}
}

func (s *Session) run() error {
	logrus.Info("=====> 获取购物车中有效商品")

	if err := s.CartAllCheck(); err != nil {
		return fmt.Errorf("全选购物车商品失败: %v", err)
	}
	cartData, err := s.GetCart()
	if err != nil {
		return err
	}

	products := cartData["products"].([]map[string]interface{})
	for k, v := range products {
		logrus.Infof("[%v] %s 数量：%v 总价：%s", k, v["product_name"], v["count"], v["total_price"])
	}

	for {
		logrus.Info("=====> 获取可预约时间")
		multiReserveTime, err := s.GetMultiReserveTime(products)
		if err != nil {
			return fmt.Errorf("获取可预约时间失败: %v", err)
		}
		if len(multiReserveTime) == 0 {
			return ErrorNoReserveTime
		}
		logrus.Infof("发现可用的配送时段!")

		logrus.Info("=====> 生成订单信息")
		checkOrderData, err := s.CheckOrder(cartData, multiReserveTime)
		if err != nil {
			return fmt.Errorf("检查订单失败: %v", err)
		}
		logrus.Infof("订单总金额：%v\n", checkOrderData["price"])

		var wg errgroup.Group
		for _, reserveTime := range multiReserveTime {
			sess := s.Clone()
			sess.SetReserve(reserveTime)
			wg.Go(func() error {
				startTime := time.Unix(int64(sess.Reserve.StartTimestamp), 0).Format("2006/01/02 15:04:05")
				endTime := time.Unix(int64(sess.Reserve.EndTimestamp), 0).Format("2006/01/02 15:04:05")
				timeRange := startTime + "——" + endTime
				logrus.Infof("=====> 提交订单中, 预约时间段(%s)", timeRange)
				if err := sess.CreateOrder(context.Background(), cartData, checkOrderData); err != nil {
					logrus.Warningf("提交订单(%s)失败: %v", timeRange, err)
					return err
				}

				s.successCh <- struct{}{}
				return nil
			})
		}
		_ = wg.Wait()
		return nil
	}
}

func (s *Session) Clone() *Session {
	return &Session{
		cfg:    s.cfg,
		client: s.client,

		channel:     s.channel,
		apiVersion:  s.apiVersion,
		appVersion:  s.appVersion,
		appClientID: s.appClientID,

		UserID:  s.UserID,
		Address: s.Address,
		PayType: s.PayType,
		Reserve: s.Reserve,
	}
}

func (s *Session) execute(ctx context.Context, request *resty.Request, method, url string) (*resty.Response, error) {
	return s.executeRetry(ctx, request, method, url, 1)
}

func (s *Session) executeRetry(ctx context.Context, request *resty.Request, method, url string, frequency int) (*resty.Response, error) {
	if ctx != nil {
		request.SetContext(ctx)
	}
	resp, err := request.Execute(method, url)
	if err != nil {
		return nil, fmt.Errorf("request failed: %v", err)
	}
	if resp.StatusCode() != http.StatusOK {
		return nil, fmt.Errorf("statusCode: %d, body: %s", resp.StatusCode(), resp.String())
	}

	result := gjson.ParseBytes(resp.Body())
	code := result.Get("code").Num
	switch code {
	case 0:
		return resp, nil
	case -3000, -3001:
		logrus.Warningf("当前人多拥挤(%v): %s", code, resp.String())
	case -3100:
		logrus.Warningf("当前页面拥挤(%v): %s", code, resp.String())
		logrus.Warningf("将在 %dms 后重试", s.cfg.Interval)
		time.Sleep(time.Duration(s.cfg.Interval) * time.Millisecond)
	default:
		if frequency > 15 {
			return nil, fmt.Errorf("无法识别的状态码: %v", resp.String())
		}
		logrus.Warningf("尝试次数: %d, 无法识别的状态码: %v", frequency, resp.String())
	}
	frequency++
	return s.executeRetry(nil, request, method, url, frequency)
}

func (s *Session) buildHeader() http.Header {
	header := make(http.Header)
	header.Set("ddmc-city-number", s.Address.CityNumber)
	header.Set("ddmc-os-version", "undefined")
	header.Set("ddmc-channel", s.channel)
	header.Set("ddmc-api-version", s.apiVersion)
	header.Set("ddmc-build-version", s.appVersion)
	header.Set("ddmc-app-client-id", s.appClientID)
	header.Set("ddmc-ip", "")
	header.Set("ddmc-station-id", s.Address.StationId)
	header.Set("ddmc-uid", s.UserID)
	if len(s.Address.Location.Location) == 2 {
		header.Set("ddmc-longitude", strconv.FormatFloat(s.Address.Location.Location[0], 'f', -1, 64))
		header.Set("ddmc-latitude", strconv.FormatFloat(s.Address.Location.Location[1], 'f', -1, 64))
	}
	return header
}

func (s *Session) buildURLParams(needAddress bool) url.Values {
	params := url.Values{}
	params.Add("channel", s.channel)
	params.Add("api_version", s.apiVersion)
	params.Add("app_version", s.appVersion)
	params.Add("app_client_id", s.appClientID)
	params.Add("applet_source", "")
	params.Add("h5_source", "")
	params.Add("sharer_uid", "")
	params.Add("s_id", "")
	params.Add("openid", "")

	params.Add("uid", s.UserID)
	if needAddress {
		params.Add("address_id", s.Address.Id)
		params.Add("station_id", s.Address.StationId)
		params.Add("city_number", s.Address.CityNumber)
		if len(s.Address.Location.Location) == 2 {
			params.Add("longitude", strconv.FormatFloat(s.Address.Location.Location[0], 'f', -1, 64))
			params.Add("latitude", strconv.FormatFloat(s.Address.Location.Location[1], 'f', -1, 64))
		}
	}

	params.Add("device_token", "")
	params.Add("nars", "")
	params.Add("sesi", "")
	return params
}

func (s *Session) SetReserve(reserve ReserveTime) {
	s.Reserve = reserve
}

func (s *Session) Choose() error {
	if err := s.chooseAddr(); err != nil {
		return err
	}
	if err := s.choosePay(); err != nil {
		return err
	}
	return nil
}

func (s *Session) chooseAddr() error {
	addrMap, err := s.GetAddress()
	if err != nil {
		return fmt.Errorf("获取收货地址失败: %v", err)
	}
	addrs := make([]string, 0, len(addrMap))
	for k := range addrMap {
		addrs = append(addrs, k)
	}

	if len(addrs) == 1 {
		address := addrMap[addrs[0]]
		s.Address = &address
		logrus.Infof("默认收货地址: %s %s", s.Address.Location.Address, s.Address.AddrDetail)
		return nil
	}

	var addr string
	sv := &survey.Select{
		Message: "请选择收货地址",
		Options: addrs,
	}
	if err := survey.AskOne(sv, &addr); err != nil {
		return fmt.Errorf("选择收货地址错误: %v", err)
	}

	address, ok := addrMap[addr]
	if !ok {
		return errors.New("请选择正确的收货地址")
	}
	s.Address = &address
	logrus.Infof("已选择收货地址: %s %s", s.Address.Location.Address, s.Address.AddrDetail)
	return nil
}

const (
	PaymentAlipay    = "alipay"
	PaymentAlipayStr = "支付宝"
	PaymentWechat    = "wechat"
	PaymentWechatStr = "微信"
)

func (s *Session) choosePay() error {
	payType := s.cfg.PayType
	if payType == "" {
		sv := &survey.Select{
			Message: "请选择支付方式",
			Options: []string{PaymentWechatStr, PaymentAlipayStr},
			Default: PaymentWechatStr,
		}
		if err := survey.AskOne(sv, &payType); err != nil {
			return fmt.Errorf("选择支付方式错误: %v", err)
		}
	}

	// 2支付宝，4微信，6小程序支付
	switch payType {
	case PaymentAlipay, PaymentAlipayStr:
		s.PayType = 2
		logrus.Info("已选择支付方式：支付宝")
	case PaymentWechat, PaymentWechatStr:
		s.PayType = 4
		logrus.Info("已选择支付方式：微信")
	default:
		return fmt.Errorf("无法识别的支付方式: %s", payType)
	}
	return nil
}
