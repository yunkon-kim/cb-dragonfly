package agent

import (
	"errors"
	"fmt"
	"github.com/cloud-barista/cb-dragonfly/pkg/types"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cloud-barista/cb-dragonfly/pkg/config"
	"github.com/cloud-barista/cb-dragonfly/pkg/util"

	"github.com/bramvdbogaerde/go-scp"
	sshrun "github.com/cloud-barista/cb-spider/cloud-control-manager/vm-ssh"
	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
)

const (
	UBUNTU = "UBUNTU"
	CENTOS = "CENTOS"
)

func InstallAgent(nsId string, mcisId string, vmId string, publicIp string, userName string, sshKey string, cspType string, port string, serviceType string) (int, error) {
	sshInfo := sshrun.SSHInfo{
		ServerPort: publicIp + ":" + port,
		UserName:   userName,
		PrivateKey: []byte(sshKey),
	}

	// {사용자계정}/cb-dragonfly 폴더 생성
	createFolderCmd := fmt.Sprintf("mkdir $HOME/cb-dragonfly")
	if _, err := sshrun.SSHRun(sshInfo, createFolderCmd); err != nil {
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to make directory cb-dragonfly, error=%s", err))
	}

	// 리눅스 OS 환경 체크
	osType, err := sshrun.SSHRun(sshInfo, "hostnamectl | grep 'Operating System' | awk '{print $3}' | tr 'a-z' 'A-Z'")
	if err != nil {
		cleanAgentInstall(sshInfo, osType)
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to check linux OS environments, error=%s", err))
	}

	rootPath := os.Getenv("CBMON_ROOT")
	// 제공 설치 파일 탐색
	filepath := rootPath + fmt.Sprintf("/file/pkg/%s/x64/", strings.ToLower(osType))
	filename, err := GetPackageName(filepath)
	if err != nil {
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to get package. osType %s not supported", osType))
	}
	sourceFile := filepath + filename

	var targetFile, installCmd string
	if strings.Contains(osType, CENTOS) {
		targetFile = fmt.Sprintf("$HOME/cb-dragonfly/cb-agent.rpm")
		installCmd = fmt.Sprintf("sudo rpm -ivh $HOME/cb-dragonfly/cb-agent.rpm")
	} else if strings.Contains(osType, UBUNTU) {
		targetFile = fmt.Sprintf("$HOME/cb-dragonfly/cb-agent.deb")
		installCmd = fmt.Sprintf("sudo dpkg -i $HOME/cb-dragonfly/cb-agent.deb")
	}

	mcisInstallFile := rootPath + fmt.Sprintf("/file/install_mcis_script.sh")
	targetmcisInstallFile := fmt.Sprintf("$HOME/cb-dragonfly/install_mcis_script.sh")

	// 에이전트 설치 패키지 다운로드
	if err := sshCopyWithTimeout(sshInfo, sourceFile, targetFile); err != nil {
		cleanAgentInstall(sshInfo, osType)
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to download agent package, error=%s", err))
	}
	// MCIS 에이전트 설치 패키지 다운로드
	if err := sshCopyWithTimeout(sshInfo, mcisInstallFile, targetmcisInstallFile); err != nil {
		cleanAgentInstall(sshInfo, osType)
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to download mcis agent package, error=%s", err))
	}

	// 패키지 설치 실행
	if _, err := sshrun.SSHRun(sshInfo, installCmd); err != nil {
		cleanAgentInstall(sshInfo, osType)
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to install agent package, error=%s", err))
	}
	cmd := "cd $HOME/cb-dragonfly && sudo chmod +x install_mcis_script.sh"
	if _, err := sshrun.SSHRun(sshInfo, cmd); err != nil {
		cleanAgentInstall(sshInfo, osType)
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to install mcis agent package, error=%s", err))
	}
	installCmd = fmt.Sprintf("cd $HOME/cb-dragonfly && ./install_mcis_script.sh")
	if _, err := sshrun.SSHRun(sshInfo, installCmd); err != nil {
		cleanAgentInstall(sshInfo, osType)
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to start installing mcis agent, error=%s", err))
	}
	sshrun.SSHRun(sshInfo, "sudo rm /etc/telegraf/telegraf.conf")

	// telegraf_conf 파일 복사
	telegrafConfSourceFile, err := createTelegrafConfigFile(nsId, mcisId, vmId, cspType)
	telegrafConfTargetFile := "$HOME/cb-dragonfly/telegraf.conf"
	if err != nil {
		cleanAgentInstall(sshInfo, osType)
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to create telegraf.conf, error=%s", err))
	}
	if err := sshrun.SSHCopy(sshInfo, telegrafConfSourceFile, telegrafConfTargetFile); err != nil {
		cleanAgentInstall(sshInfo, osType)
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to copy telegraf.conf, error=%s", err))
	}

	if _, err := sshrun.SSHRun(sshInfo, "sudo mv $HOME/cb-dragonfly/telegraf.conf /etc/telegraf/"); err != nil {
		cleanAgentInstall(sshInfo, osType)
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to move telegraf.conf, error=%s", err))
	}

	// 카프카 도메인 정보 기입 /etc/hosts => agent에서 도메인 등록하도록 기능 변경
	inputKafkaServerDomain := fmt.Sprintf("echo '%s %s' | sudo tee -a /etc/hosts", config.GetInstance().Dragonfly.DragonflyIP, "cb-dragonfly-kafka")
	_, err = sshrun.SSHRun(sshInfo, inputKafkaServerDomain)
	if err != nil {
		cleanAgentInstall(sshInfo, osType)
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to register kafka domain, error=%s", err))
	}

	var dragonflyPort string
	if config.GetInstance().Monitoring.DeployType == types.Helm {
		dragonflyPort = strconv.Itoa(config.GetInstance().Dragonfly.HelmPort)
	} else {
		dragonflyPort = strconv.Itoa(config.GetInstance().Dragonfly.Port)
	}

	inputDragonflyDomain := fmt.Sprintf("echo '%s %s' | sudo tee -a /etc/hosts", config.GetInstance().Dragonfly.DragonflyIP+":"+dragonflyPort, "cb-dragonfly")
	_, err = sshrun.SSHRun(sshInfo, inputDragonflyDomain)
	if err != nil {
		cleanAgentInstall(sshInfo, osType)
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to register kafka domain, error=%s", err))
	}

	// 공통 서비스 활성화 및 실행
	if _, err := sshrun.SSHRun(sshInfo, "sudo systemctl enable telegraf && sudo systemctl restart telegraf"); err != nil {
		cleanAgentInstall(sshInfo, osType)
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to enable and start telegraf service, error=%s", err))
	}

	// telegraf UUId conf 파일 삭제
	err = os.Remove(telegrafConfSourceFile)
	if err != nil {
		cleanAgentInstall(sshInfo, osType)
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to remove temporary telegraf.conf file, error=%s", err))
	}

	// 에이전트 설치에 사용한 파일 폴더 채로 제거
	removeRpmCmd := fmt.Sprintf("sudo rm -rf $HOME/cb-dragonfly")
	if _, err := sshrun.SSHRun(sshInfo, removeRpmCmd); err != nil {
		cleanAgentInstall(sshInfo, osType)
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to remove cb-dragonfly directory, error=%s", err))
	}

	// 정상 설치 확인
	checkCmd := "telegraf --version"
	if result, err := sshrun.SSHRun(sshInfo, checkCmd); err != nil {
		cleanAgentInstall(sshInfo, osType)
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to run telegraf command, error=%s", err))
	} else {
		if strings.Contains(result, "command not found") {
			cleanAgentInstall(sshInfo, osType)
			return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to run telegraf command, error=%s", err))
		}
	}

	// 에이전트 권한 변경
	stopcmd := fmt.Sprintf("sudo systemctl stop telegraf && sudo usermod -u 0 -o telegraf && sudo systemctl restart telegraf")
	if _, err := sshrun.SSHRun(sshInfo, stopcmd); err != nil {
		cleanAgentInstall(sshInfo, osType)
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to change telegraf permission, err=%s", err))
	}

	// 메타데이터 저장
	_, _, err = PutAgent(nsId, mcisId, vmId, cspType, publicIp, false, serviceType)
	if err != nil {
		cleanAgentInstall(sshInfo, osType)
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to put metadata to cb-store, error=%s", err))
	}

	return http.StatusOK, nil
}

func cleanAgentInstall(sshInfo sshrun.SSHInfo, osType string) {
	// Uninstall Telegraf
	var uninstallCmd string
	if strings.Contains(osType, CENTOS) {
		uninstallCmd = fmt.Sprintf("sudo rpm -e telegraf")
	} else if strings.Contains(osType, UBUNTU) {
		uninstallCmd = fmt.Sprintf("sudo dpkg -r telegraf")
	}
	sshrun.SSHRun(sshInfo, uninstallCmd)

	// Delete Install Files
	removeRpmCmd := fmt.Sprintf("sudo rm -rf $HOME/cb-dragonfly")
	sshrun.SSHRun(sshInfo, removeRpmCmd)
	removeDirCmd := fmt.Sprintf("sudo rm -rf /etc/telegraf/telegraf.conf")
	sshrun.SSHRun(sshInfo, removeDirCmd)
}

func createTelegrafConfigFile(nsId string, mcisId string, vmId string, cspType string) (string, error) {
	mechanism := fmt.Sprintf(strings.ToLower(config.GetInstance().Monitoring.DefaultPolicy))
	rootPath := os.Getenv("CBMON_ROOT")
	filePath := rootPath + "/file/conf/telegraf.conf"

	read, err := ioutil.ReadFile(filePath)
	if err != nil {
		// ERROR 정보 출럭
		util.GetLogger().Error("failed to read telegraf.conf file.")
		return "", err
	}

	// 파일 내의 변수 값 설정 (hostId, collectorServer)
	strConf := string(read)
	strConf = strings.ReplaceAll(strConf, "{{ns_id}}", nsId)
	strConf = strings.ReplaceAll(strConf, "{{mcis_id}}", mcisId)
	strConf = strings.ReplaceAll(strConf, "{{vm_id}}", vmId)
	strConf = strings.ReplaceAll(strConf, "{{csp_type}}", cspType)
	strConf = strings.ReplaceAll(strConf, "{{mechanism}}", mechanism)

	strConf = strings.ReplaceAll(strConf, "{{topic}}", fmt.Sprintf("%s_%s_%s_%s", nsId, mcisId, vmId, cspType))
	var kafkaPort int
	if config.GetInstance().GetMonConfig().DeployType == types.Helm {
		kafkaPort = config.GetInstance().Kafka.HelmPort
	} else {
		kafkaPort = types.KafkaDefaultPort
	}
	kafkaAddr := fmt.Sprintf("%s:%d", config.GetInstance().Kafka.EndpointUrl, kafkaPort)
	strConf = strings.ReplaceAll(strConf, "{{broker_server}}", kafkaAddr)

	// telegraf.conf 파일 생성
	telegrafFilePath := rootPath + "/file/conf/"
	createFileName := "telegraf-" + uuid.New().String() + ".conf"
	telegrafConfFile := telegrafFilePath + createFileName

	err = ioutil.WriteFile(telegrafConfFile, []byte(strConf), os.FileMode(777))
	if err != nil {
		util.GetLogger().Error("failed to create telegraf.conf file.")
		return "", err
	}
	return telegrafConfFile, err
}

func sshCopyWithTimeout(sshInfo sshrun.SSHInfo, sourceFile string, targetFile string) error {
	signer, err := ssh.ParsePrivateKey(sshInfo.PrivateKey)
	if err != nil {
		return err
	}
	clientConfig := ssh.ClientConfig{
		User: sshInfo.UserName,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	client := scp.NewClientWithTimeout(sshInfo.ServerPort, &clientConfig, 600*time.Second)
	err = client.Connect()
	defer client.Close()
	if err != nil {
		return err
	}

	file, err := os.Open(sourceFile)
	defer file.Close()
	if err != nil {
		return err
	}

	return client.CopyFile(file, targetFile, "0755")
}

func GetPackageName(path string) (string, error) {
	file, err := ioutil.ReadDir(path)
	var filename string
	for _, data := range file {
		filename = data.Name()
	}
	return filename, err
}

// 전체 에이전트 삭제 테스트용 코드
func UninstallAgent(
	nsId string,
	mcisId string,
	vmId string,
	publicIp string,
	userName string,
	sshKey string,
	cspType string,
	port string,
) (int, error) {
	var err error
	sshInfo := sshrun.SSHInfo{
		ServerPort: publicIp + ":" + port,
		UserName:   userName,
		PrivateKey: []byte(sshKey),
	}

	// {사용자계정}/cb-dragonfly 폴더 생성
	createFolderCmd := fmt.Sprintf("mkdir $HOME/cb-dragonfly")
	if _, err := sshrun.SSHRun(sshInfo, createFolderCmd); err != nil {
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to make directory cb-dragonfly, error=%s", err))
	}

	// 리눅스 OS 환경 체크
	osType, err := sshrun.SSHRun(sshInfo, "hostnamectl | grep 'Operating System' | awk '{print $3}' | tr 'a-z' 'A-Z'")
	if err != nil {
		cleanAgentInstall(sshInfo, osType)
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to check linux OS environments, error=%s", err))
	}

	rootPath := os.Getenv("CBMON_ROOT")
	// 제공 설치 파일 탐색
	sourceFile := rootPath + fmt.Sprintf("/file/uninstall_mcis_script.sh")

	var targetFile, Cmd string
	targetFile = fmt.Sprintf("$HOME/cb-dragonfly/uninstall_mcis_script.sh")

	// 에이전트 설치 패키지 다운로드
	if err := sshCopyWithTimeout(sshInfo, sourceFile, targetFile); err != nil {
		cleanAgentInstall(sshInfo, osType)
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to download agent package, error=%s", err))
	}
	cmd := "cd $HOME/cb-dragonfly && sudo chmod +x uninstall_mcis_script.sh"
	if _, err := sshrun.SSHRun(sshInfo, cmd); err != nil {
		cleanAgentInstall(sshInfo, osType)
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to chmod agent package, error=%s", err))
	}
	Cmd = fmt.Sprintf("cd $HOME/cb-dragonfly && ./uninstall_mcis_script.sh")
	if _, err := sshrun.SSHRun(sshInfo, Cmd); err != nil {
		cleanAgentInstall(sshInfo, osType)
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to uninstall agent, error=%s", err))
	}
	// sudo perl -pi -e "s,^192.168.130.14.*tml\n$,," /etc/hosts

	Cmd = fmt.Sprintf("sudo perl -pi -e 's,^%s.*%s\n$,,' /etc/hosts", config.GetInstance().Dragonfly.DragonflyIP, "cb-dragonfly-kafka")
	if _, err := sshrun.SSHRun(sshInfo, Cmd); err != nil {
		cleanAgentInstall(sshInfo, osType)
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to delete domain list, error=%s", err))
	}

	var dragonflyPort string
	if config.GetInstance().Monitoring.DeployType == types.Helm {
		dragonflyPort = strconv.Itoa(config.GetInstance().Dragonfly.HelmPort)
	} else {
		dragonflyPort = strconv.Itoa(config.GetInstance().Dragonfly.Port)
	}

	Cmd = fmt.Sprintf("sudo perl -pi -e 's,^%s.*%s\n$,,' /etc/hosts", config.GetInstance().Dragonfly.DragonflyIP+":"+dragonflyPort, "cb-dragonfly")
	if _, err := sshrun.SSHRun(sshInfo, Cmd); err != nil {
		cleanAgentInstall(sshInfo, osType)
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to delete domain list, error=%s", err))
	}

	// 에이전트 설치에 사용한 파일 폴더 채로 제거
	cleanAgentInstall(sshInfo, osType)

	// 메타데이터 삭제
	err = DeleteAgent(nsId, mcisId, vmId, cspType)
	if err != nil {
		return http.StatusInternalServerError, errors.New(fmt.Sprintf("failed to delete metadata, error=%s", err))
	}

	//// Topic Queue 등록
	//if config.GetInstance().GetMonConfig().DefaultPolicy == types.PushPolicy {
	//	if err = util.RingQueuePut(types.TopicDel, fmt.Sprintf("%s_%s_%s_%s", nsId, mcisId, vmId, cspType)); err != nil {
	//		util.GetLogger().Error(err)
	//	}
	//}
	return http.StatusOK, nil

}
