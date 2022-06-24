package rtsync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/gotomicro/ego/core/econf"

	"github.com/clickvisual/clickvisual/api/internal/invoker"
)

func init() {
	wd, _ := os.Getwd()
	for !strings.HasSuffix(wd, "clickvisual") {
		wd = filepath.Dir(wd)
	}
	fmt.Println("path: ", wd+"/configs/local.toml")
	f, err := os.Open(wd + "/configs/local.toml")
	if err != nil {
		panic(err)
	}
	defer func() { _ = f.Close() }()
	err = econf.LoadFromReader(f, toml.Unmarshal)
	if err != nil {
		panic(err)
	}
	invoker.Init()
}

func TestCreator(t *testing.T) {
	type args struct {
		iid     int
		content string
	}
	tests := []struct {
		name    string
		args    args
		want    RTSync
		wantErr bool
	}{
		// TODO: Add test cases.
		{
			name: "test-1",
			args: args{
				iid: 1,
				content: `{
    "source": {
        "typ": "clickhouse",
        "database": "metrics",
        "table": "samples"
    },
    "target": {
        "typ": "mysql",
        "sourceId": 3,
        "database": "ws_gateway",
        "table": "number_sender"
    },
    "mapping": [
        {
            "source": "name",
            "target": "gateway_ip"
        },{
            "source": "val",
            "target": "online"
        }
    ]
}`,
			},
			want:    nil,
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Creator(tt.args.iid, tt.args.content)
			if (err != nil) != tt.wantErr {
				t.Errorf("Creator() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			_, err = got.OkGoGoGo()
			if (err != nil) != tt.wantErr {
				t.Errorf("OkGoGoGo() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
		})
	}
}
