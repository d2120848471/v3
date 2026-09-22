package engine

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/d2120848471/v3/ali-slider-go/internal/domain/baxia"
)

const baxiaTestAWSCURL = "https://g.alicdn.com/AWSC/awsc.js"

func TestBaxiaDiscoveryValidatesInputs(t *testing.T) {
	config := baxiaTestConfig(t)
	config.SDKSource = nil // 发现 SDK 时还没有 SDK source。
	for _, tc := range []struct {
		name   string
		url    string
		source []byte
	}{
		{"missing URL", "", []byte("fixture")},
		{"relative URL", "/awsc.js", []byte("fixture")},
		{"source credentials", "https://user:secret@g.alicdn.com/AWSC/awsc.js", []byte("fixture")},
		{"empty AWSC", baxiaTestAWSCURL, nil},
		{"invalid UTF-8", baxiaTestAWSCURL, []byte{0xff}},
		{"large AWSC", baxiaTestAWSCURL, make([]byte, baxiaMaximumSDKBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DiscoverBaxiaSDK(context.Background(), config, tc.url, tc.source); !errors.Is(err, baxia.ErrConfig) {
				t.Fatalf("discovery validation = %v", err)
			}
		})
	}
	//lint:ignore SA1012 此处故意传 nil，验证边界拒绝空 context。
	if _, err := DiscoverBaxiaSDK(nil, config, baxiaTestAWSCURL, []byte("fixture")); !errors.Is(err, baxia.ErrConfig) {
		t.Fatalf("nil context = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DiscoverBaxiaSDK(ctx, config, baxiaTestAWSCURL, []byte("fixture")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled discovery = %v", err)
	}
}

func TestBaxiaNativeAWSCSelectsScriptFromPageAndSource(t *testing.T) {
	config := baxiaNativeConfig(t)
	config.SDKSource = nil
	source := []byte(`
      (() => {
        const parent = document.getElementsByTagName("script")[0];
        if (navigator.userAgent.indexOf("Chrome") < 0 || location.hostname !== "www.galaxyticketing.com") throw new Error("wrong browser profile");
        if (document.URL !== location.href || document.domain !== location.hostname) throw new Error("wrong document location");
        if (!parent.hasAttribute("src") || parent.src !== "https://assets.example.test/??AWSC/awsc.js") throw new Error("wrong AWSC source URL");
        const insert = (id, src) => {
          const node = document.createElement("script");
          node.id = id; node.src = src;
          node.onload = () => { throw new Error("must not load discovered scripts"); };
          parent.parentNode.insertBefore(node, parent);
        };
        insert("AWSC_etModule", "https://telemetry.example.test/et.js");
        setTimeout(() => { throw new Error("discovery must not run load timers"); }, 0);
        AWSC = { use(feature) {
          if (feature !== "fy") throw new Error("wrong AWSC feature");
          insert("AWSC_fyModule", "/AWSC/fireyejs/1.777.1/fireyejs.js");
        }};
      })();
    `)
	got, err := DiscoverBaxiaSDK(context.Background(), config, "https://assets.example.test/??AWSC/awsc.js", source)
	if err != nil || got != "https://assets.example.test/AWSC/fireyejs/1.777.1/fireyejs.js" {
		t.Fatalf("AWSC script selection = %s, %v", got, err)
	}
}

func TestBaxiaNativeAWSCRejectsUnsupportedSelection(t *testing.T) {
	config := baxiaNativeConfig(t)
	for _, tc := range []struct{ name, source string }{
		{"missing use", `AWSC={}`},
		{"no Fireye", `AWSC={use(){}}`},
		{"multiple Fireye URLs", `AWSC={use(){for(const version of [231,234]){const script=document.createElement("script");script.id="AWSC_fyModule";script.src="https://g.alicdn.com/AWSC/fireyejs/1."+version+".1/fireyejs.js";document.head.appendChild(script)}}}`},
		{"non-Fireye script", `AWSC={use(){const script=document.createElement("script");script.id="AWSC_fyModule";script.src="https://example.test/et.js";document.head.appendChild(script)}}`},
		{"insecure Fireye", `AWSC={use(){const script=document.createElement("script");script.id="AWSC_fyModule";script.src="http://example.test/fireyejs.js";document.head.appendChild(script)}}`},
		{"wrong module id", `AWSC={use(){const script=document.createElement("script");script.id="AWSC_etModule";script.src="https://example.test/fireyejs.js";document.head.appendChild(script)}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DiscoverBaxiaSDK(context.Background(), config, baxiaTestAWSCURL, []byte(tc.source))
			if got != "" || !errors.Is(err, baxia.ErrUnsupportedSDK) {
				t.Fatalf("unsupported discovery = %s, %v", got, err)
			}
		})
	}
}

func TestBaxiaNativeOriginalAWSCDiscovery(t *testing.T) {
	config := baxiaNativeConfig(t)
	path := os.Getenv("ALI_BAXIA_TEST_AWSC")
	if path == "" {
		t.Skip("ALI_BAXIA_TEST_AWSC is not set")
	}
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DiscoverBaxiaSDK(context.Background(), config, baxiaTestAWSCURL, source)
	if err != nil || !strings.HasPrefix(got, "https://g.alicdn.com/AWSC/fireyejs/") || !strings.HasSuffix(got, "/fireyejs.js") {
		t.Fatalf("original AWSC selection = %s, %v", got, err)
	}
	t.Logf("original AWSC selected %s", got)
}
