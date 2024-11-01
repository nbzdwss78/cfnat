package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	tracePath   = "/cdn-cgi/trace" // trace URL路径
	timeout     = 1 * time.Second  // 超时时间
	maxDuration = 2 * time.Second  // 最大持续时间
)

var (
	ipsType    = flag.String("ips", "4", "指定生成IPv4还是IPv6地址 (4或6)")                      // IP类型（IPv4 或 IPv6）
	outFile    = flag.String("outfile", "ip.csv", "输出文件名称")                             // 输出文件名称
	maxThreads = flag.Int("task", 100, "并发请求最大协程数")                                     // 最大协程数
	random     = flag.Bool("random", true, "是否随机生成IP（默认为true），如果为false，则从CIDR中拆分出所有IP") // 是否随机
)

type result struct {
	ip          string        // IP地址
	dataCenter  string        // 数据中心
	region      string        // 地区
	city        string        // 城市
	latency     string        // 延迟
	tcpDuration time.Duration // TCP请求延迟
}

type location struct {
	Iata   string  `json:"iata"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Cca2   string  `json:"cca2"`
	Region string  `json:"region"`
	City   string  `json:"city"`
}

func main() {
	flag.Parse()
	startTime := time.Now()

	var locations []location
	if _, err := os.Stat("locations.json"); os.IsNotExist(err) {
		fmt.Println("本地 locations.json 不存在\n正在从 https://speed.cloudflare.com/locations 下载 locations.json")
		resp, err := http.Get("https://speed.cloudflare.com/locations")
		if err != nil {
			fmt.Printf("无法从URL中获取JSON: %v\n", err)
			return
		}

		defer resp.Body.Close()

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			fmt.Printf("无法读取响应体: %v\n", err)
			return
		}

		err = json.Unmarshal(body, &locations)
		if err != nil {
			fmt.Printf("无法解析JSON: %v\n", err)
			return
		}
		file, err := os.Create("locations.json")
		if err != nil {
			fmt.Printf("无法创建文件: %v\n", err)
			return
		}
		defer file.Close()

		_, err = file.Write(body)
		if err != nil {
			fmt.Printf("无法写入文件: %v\n", err)
			return
		}
	} else {
		fmt.Println("本地 locations.json 已存在,无需重新下载")
		file, err := os.Open("locations.json")
		if err != nil {
			fmt.Printf("无法打开文件: %v\n", err)
			return
		}
		defer file.Close()

		body, err := ioutil.ReadAll(file)
		if err != nil {
			fmt.Printf("无法读取文件: %v\n", err)
			return
		}

		err = json.Unmarshal(body, &locations)
		if err != nil {
			fmt.Printf("无法解析JSON: %v\n", err)
			return
		}
	}

	locationMap := make(map[string]location)
	for _, loc := range locations {
		locationMap[loc.Iata] = loc
	}

	var url string
	var filename string

	if *ipsType == "6" {
		filename = "ips-v6.txt"
		url = "https://www.baipiao.eu.org/cloudflare/ips-v6"
	} else if *ipsType == "4" {
		filename = "ips-v4.txt"
		url = "https://www.baipiao.eu.org/cloudflare/ips-v4"
	} else {
		fmt.Println("无效的IP类型。请使用 '4' 或 '6'")
		return
	}

	var content string
	var err error

	// 检查本地是否有文件
	if _, err = os.Stat(filename); os.IsNotExist(err) {
		// 如果文件不存在，从URL获取数据
		fmt.Printf("文件 %s 不存在，正在从 URL %s 下载数据\n", filename, url)
		content, err = getURLContent(url)
		if err != nil {
			fmt.Println("获取URL内容出错:", err)
			return
		}
		// 保存数据到本地文件
		err = saveToFile(filename, content)
		if err != nil {
			fmt.Println("保存文件出错:", err)
			return
		}
	} else {
		// 如果文件存在，从本地文件读取数据
		content, err = getFileContent(filename)
		if err != nil {
			fmt.Println("读取本地文件出错:", err)
			return
		}
	}

	var ipList []string
	if *random {
		ipList = parseIPList(content)
		var randomIPs []string
		if *ipsType == "6" {
			randomIPs = getRandomIPv6s(ipList)
		} else {
			randomIPs = getRandomIPv4s(ipList)
		}
		ipList = randomIPs
	} else {
		// 从CIDR中拆分出所有IP
		ipList, err = readIPs(filename)
		if err != nil {
			fmt.Println("读取IP出错:", err)
			return
		}
	}

	// 从生成的 IP 列表进行处理
	var wg sync.WaitGroup
	wg.Add(len(ipList))

	resultChan := make(chan result, len(ipList))

	thread := make(chan struct{}, *maxThreads)

	var count int
	total := len(ipList)

	for _, ip := range ipList {
		thread <- struct{}{}
		go func(ip string) {
			defer func() {
				<-thread
				wg.Done()
				count++
				percentage := float64(count) / float64(total) * 100
				fmt.Printf("已完成: %d 总数: %d 已完成: %.2f%%\r", count, total, percentage)
				if count == total {
					fmt.Printf("已完成: %d 总数: %d 已完成: %.2f%%\n", count, total, percentage)
				}
			}()

			dialer := &net.Dialer{
				Timeout:   timeout,
				KeepAlive: 0,
			}
			start := time.Now()
			conn, err := dialer.Dial("tcp", net.JoinHostPort(ip, "80"))
			if err != nil {
				return
			}
			defer conn.Close()

			tcpDuration := time.Since(start)
			start = time.Now()

			client := http.Client{
				Transport: &http.Transport{
					Dial: func(network, addr string) (net.Conn, error) {
						return conn, nil
					},
				},
				Timeout: timeout,
			}

			requestURL := "http://" + net.JoinHostPort(ip, "80") + tracePath

			req, _ := http.NewRequest("GET", requestURL, nil)

			req.Header.Set("User-Agent", "Mozilla/5.0")
			req.Close = true
			resp, err := client.Do(req)
			if err != nil {
				return
			}

			duration := time.Since(start)
			if duration > maxDuration {
				return
			}

			buf := &bytes.Buffer{}
			timeout := time.After(maxDuration)
			done := make(chan bool)
			go func() {
				_, err := io.Copy(buf, resp.Body)
				done <- true
				if err != nil {
					return
				}
			}()
			select {
			case <-done:
			case <-timeout:
				return
			}

			body := buf
			if err != nil {
				return
			}

			if strings.Contains(body.String(), "uag=Mozilla/5.0") {
				if matches := regexp.MustCompile(`colo=([A-Z]+)`).FindStringSubmatch(body.String()); len(matches) > 1 {
					dataCenter := matches[1]
					loc, ok := locationMap[dataCenter]
					if ok {
						fmt.Printf("发现有效IP %s 位置信息 %s 延迟 %d 毫秒\n", ip, loc.City, tcpDuration.Milliseconds())
						resultChan <- result{ip, dataCenter, loc.Region, loc.City, fmt.Sprintf("%d ms", tcpDuration.Milliseconds()), tcpDuration}
					} else {
						fmt.Printf("发现有效IP %s 位置信息未知 延迟 %d 毫秒\n", ip, tcpDuration.Milliseconds())
						resultChan <- result{ip, dataCenter, "", "", fmt.Sprintf("%d ms", tcpDuration.Milliseconds()), tcpDuration}
					}
				}
			}
		}(ip)
	}

	wg.Wait()
	close(resultChan)

	if len(resultChan) == 0 {
		fmt.Print("\033[2J")
		fmt.Println("未发现有效IP")
		return
	}

	var results []result
	for r := range resultChan {
		results = append(results, r)
	}

	// 按 TCP 延迟排序
	sort.Slice(results, func(i, j int) bool {
		return results[i].tcpDuration < results[j].tcpDuration
	})

	file, err := os.Create(*outFile)
	if err != nil {
		fmt.Printf("无法创建文件: %v\n", err)
		return
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	writer.Write([]string{"IP地址", "数据中心", "地区", "城市", "网络延迟"})
	for _, res := range results {
		writer.Write([]string{res.ip, res.dataCenter, res.region, res.city, res.latency})
	}

	writer.Flush()
	if err := writer.Error(); err != nil {
		fmt.Printf("写入CSV文件时出现错误: %v\n", err)
		return
	}

	fmt.Printf("成功将结果写入文件 %s，耗时 %d秒\n", *outFile, time.Since(startTime)/time.Second)
}

// 获取URL内容
func getURLContent(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP请求失败，状态码: %d", resp.StatusCode)
	}

	var content strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		content.WriteString(scanner.Text() + "\n")
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	return content.String(), nil
}

// 从本地文件读取内容
func getFileContent(filename string) (string, error) {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// 将内容保存到本地文件
func saveToFile(filename, content string) error {
	return ioutil.WriteFile(filename, []byte(content), 0644)
}

// 解析IP列表
func parseIPList(content string) []string {
	scanner := bufio.NewScanner(strings.NewReader(content))
	var ipList []string
	for scanner.Scan() {
		ipList = append(ipList, scanner.Text())
	}
	return ipList
}

// 从每个/24子网随机提取一个IPv4
func getRandomIPv4s(ipList []string) []string {
	rand.Seed(time.Now().UnixNano())
	var randomIPs []string
	for _, subnet := range ipList {
		baseIP := strings.TrimSuffix(subnet, "/24")
		octets := strings.Split(baseIP, ".")
		octets[3] = fmt.Sprintf("%d", rand.Intn(256))
		randomIP := strings.Join(octets, ".")
		randomIPs = append(randomIPs, randomIP)
	}
	return randomIPs
}

// 从每个/48子网随机提取一个IPv6
func getRandomIPv6s(ipList []string) []string {
	rand.Seed(time.Now().UnixNano())
	var randomIPs []string
	for _, subnet := range ipList {
		baseIP := strings.TrimSuffix(subnet, "/48")
		sections := strings.Split(baseIP, ":")
		sections = sections[:3] // 保留前三组
		for i := 3; i < 8; i++ {
			sections = append(sections, fmt.Sprintf("%x", rand.Intn(65536)))
		}
		randomIP := strings.Join(sections, ":")
		randomIPs = append(randomIPs, randomIP)
	}
	return randomIPs
}

// 从CIDR中拆分出所有IP
func readIPs(filename string) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var ips []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// 检查是否是CIDR格式
		if strings.Contains(line, "/") {
			// 获取CIDR中的所有IP
			ip, ipNet, err := net.ParseCIDR(line)
			if err != nil {
				return nil, err
			}
			for ip := ip.Mask(ipNet.Mask); ipNet.Contains(ip); incrementIP(ip) {
				ips = append(ips, ip.String())
			}
		} else {
			// 直接加入IP地址
			ips = append(ips, line)
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return ips, nil
}

// 增加IP
func incrementIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}
