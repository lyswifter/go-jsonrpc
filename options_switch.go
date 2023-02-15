package jsonrpc

import (
	"bufio"
	"io"
	"os"
	"strings"

	"golang.org/x/xerrors"
)

func WithSwitchConnect(infos []string) func(c *Config) {
	return func(c *Config) {
		var tmp []SwitchConnectInfo
		for _, info := range infos {
			apiinfo := ParseApiInfo(info)
			tmp = append(tmp, SwitchConnectInfo{
				Addr:   apiinfo.Addr,
				Header: apiinfo.AuthHeader(),
			})
		}
		if len(tmp) == 0 {
			return
		}
		c.switchConnect = tmp
	}
}

func WithSwitchFile(path string) func(c *Config) {
	return func(c *Config) {
		c.switchFile = path
	}
}

func WithClientType(t string) func(c *Config) {
	return func(c *Config) {
		c.clientType = t
	}
}

func readline(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	rd := bufio.NewReader(f)
	var ret = []string{}
	for {
		line, err := rd.ReadString('\n') //以'\n'为结束符读入一行
		if err != nil || io.EOF == err {
			break
		}
		ret = append(ret, line)
	}
	return ret
}

func parseAddr(path string) ([]SwitchConnectInfo, error) {
	defer func() {
		log.Infof("auth api info path: %s", path)
	}()
	bts := readline(path)
	if len(bts) == 0 {
		return nil, xerrors.Errorf("read target len: %v", bts)
	}
	var tmp []SwitchConnectInfo
	for _, info := range bts {
		info = strings.TrimSuffix(info, "\n")
		if strings.Contains(info, "#") {
			info = strings.Split(info, "#")[0]
		}
		ainfo := ParseApiInfo(info)
		addr, err := ainfo.DialArgs()
		if err != nil {
			return nil, err
		}
		tmp = append(tmp, SwitchConnectInfo{
			Addr:   addr,
			Header: ainfo.AuthHeader(),
		})
	}
	return tmp, nil
}
