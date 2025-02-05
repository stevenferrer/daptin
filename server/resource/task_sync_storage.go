package resource

import (
	"context"
	"encoding/json"
	"github.com/artpar/rclone/cmd"
	"github.com/artpar/rclone/fs/config"
	"github.com/artpar/rclone/fs/sync"
	"github.com/artpar/rclone/lib/pacer"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"strings"
)

func (res *DbResource) SyncStorageToPath(cloudStore CloudStore, cloudPath string, tempDirectoryPath string) error {

	oauthTokenId := cloudStore.OAutoTokenId

	token, oauthConf, err := res.GetTokenByTokenReferenceId(oauthTokenId)
	CheckErr(err, "Failed to get oauth2 token for scheduled storage sync")
	if err != nil {
		log.Printf("Storage syncing will fail without valid token: OAuthTokenID [%v]", oauthTokenId)
		return err
	}

	//hostRouter := httprouter.New()

	jsonToken, err := json.Marshal(token)
	CheckErr(err, "Failed to convert token to json")
	config.FileSet(cloudStore.StoreProvider, "client_id", oauthConf.ClientID)
	config.FileSet(cloudStore.StoreProvider, "type", cloudStore.StoreProvider)
	config.FileSet(cloudStore.StoreProvider, "client_secret", oauthConf.ClientSecret)
	config.FileSet(cloudStore.StoreProvider, "token", string(jsonToken))
	config.FileSet(cloudStore.StoreProvider, "client_scopes", strings.Join(oauthConf.Scopes, ","))
	config.FileSet(cloudStore.StoreProvider, "redirect_url", oauthConf.RedirectURL)

	args := []string{
		cloudStore.RootPath,
		tempDirectoryPath,
	}

	if cloudPath != "" {
		args[0] = args[0] + "/" + cloudPath
	}

	fsrc, fdst := cmd.NewFsSrcDst(args)
	pacer1 := pacer.Pacer{}
	pacer1.SetRetries(5)
	log.Infof("Temp dir for site [%v]/%v ==> %v", cloudStore.Name, cloudStore.RootPath, tempDirectoryPath)
	go cmd.Run(true, true, &cobra.Command{
		Use: "FolderSync",
	}, func() error {
		if fsrc == nil || fdst == nil {
			log.Errorf("Either source or destination is empty")
			return nil
		}
		ctx := context.Background()
		log.Infof("Starting to copy drive for site base from [%v] to [%v]", fsrc.String(), fdst.String())
		if fsrc == nil || fdst == nil {
			log.Errorf("Source or destination is null")
			return nil
		}
		dir := sync.CopyDir(ctx, fdst, fsrc, true)
		return dir
	})

	return nil
}
