package slider_test

import (
	"context"
	"fmt"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/pkg/slider"
)

func ExampleNewClient() {
	options := slider.DefaultClientOptions()
	options.Timeout = 15 * time.Second
	options.MinimumConfidence = 0 // 从默认配置覆盖，保留显式零值。
	client, err := slider.NewClient(options)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer client.Close()

	// 构造器不访问上游或加载 V8；应用可以在多个请求间复用 client。
	fmt.Println("客户端已创建")
	// Output: 客户端已创建
}

// offlineSolver 演示宿主测试如何实现公共接口，不访问上游或加载 V8。
type offlineSolver struct{}

func (offlineSolver) Solve(_ context.Context, request slider.Request) (slider.Result, error) {
	return slider.Result{
		OK: true, SecurityToken: "offline-example-token",
		VerifyCode: "T001", VerifyResult: true,
		CertifyID: "offline-example", SceneID: request.SceneID,
		TimingsMS: map[string]int{"total": 0},
	}, nil
}

func ExampleSolver() {
	var solver slider.Solver = offlineSolver{}
	result, err := solver.Solve(context.Background(), slider.Request{SceneID: "example-scene"})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(result.OK, result.VerifyCode)
	// Output: true T001
}
