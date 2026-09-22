package baxia_test

import (
	"context"
	"fmt"

	"github.com/d2120848471/v3/ali-slider-go/pkg/baxia"
)

func ExampleClient_NewSession() {
	run := func(ctx context.Context) error {
		client, err := baxia.NewClient(baxia.ClientOptions{})
		if err != nil {
			return err
		}
		defer client.Close()
		profile, err := baxia.GenerateProfile()
		if err != nil {
			return err
		}
		session, err := client.NewSession(ctx, baxia.Config{
			V8LibraryPath: "/opt/ali-slider/libali_slider_v8_runtime.so",
			Profile:       profile,
			PageURL:       "https://example.com/login",
		})
		if err != nil {
			return err
		}
		defer session.Close()
		result, err := session.Token(ctx, "https://example.com/api/login")
		if err != nil {
			return err
		}
		fmt.Println(result.Token)
		return nil
	}
	if err := run(context.Background()); err != nil {
		fmt.Println(err)
	}
}
