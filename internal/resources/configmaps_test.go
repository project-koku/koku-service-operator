package resources

import (
	"strings"
	"testing"
)

func TestDBInitConfigMap(t *testing.T) {
	cfg := testCfg()
	cm := DBInitConfigMap(cfg)
	if cm.Name != NameDBInitConfigMap(cfg) {
		t.Errorf("Name = %q", cm.Name)
	}
	script, ok := cm.Data["init-databases.sh"]
	if !ok || script == "" {
		t.Fatal("missing init-databases.sh")
	}
	for _, want := range []string{
		"costonprem_ros",
		"costonprem_koku",
		"costonprem_rbac",
		"costonprem_kruize",
		"$ROS_USER",
		"$KOKU_USER",
		"pg_stat_statements",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("init script missing %q", want)
		}
	}
}

func TestAWSConfigMap_Defaults(t *testing.T) {
	cfg := testCfg()
	cm := AWSConfigMap(cfg)
	if cm.Name != NameAWSConfigMap(cfg) {
		t.Errorf("Name = %q", cm.Name)
	}
	cfgData := cm.Data["config"]
	if !strings.Contains(cfgData, "region = onprem") {
		t.Errorf("default region missing: %q", cfgData)
	}
	if !strings.Contains(cfgData, "addressing_style = path") {
		t.Errorf("default addressing_style missing: %q", cfgData)
	}
}

func TestAWSConfigMap_Overrides(t *testing.T) {
	cfg := testCfg()
	cfg.Spec.ObjectStorage.S3.Region = "us-east-1"
	cfg.Spec.ObjectStorage.S3.AddressingStyle = "virtual"
	cm := AWSConfigMap(cfg)
	cfgData := cm.Data["config"]
	if !strings.Contains(cfgData, "region = us-east-1") {
		t.Errorf("region override missing: %q", cfgData)
	}
	if !strings.Contains(cfgData, "addressing_style = virtual") {
		t.Errorf("addressing_style override missing: %q", cfgData)
	}
}

func TestCACombineConfigMap(t *testing.T) {
	cfg := testCfg()
	cm := CACombineConfigMap(cfg)
	if cm.Name != NameCACombineConfigMap(cfg) {
		t.Errorf("Name = %q", cm.Name)
	}
	script := cm.Data["combine-ca.sh"]
	if !strings.Contains(script, "/ca-output/ca-bundle.crt") {
		t.Errorf("combine script missing output path: %q", script)
	}
}

func TestServiceCAConfigMap_InjectAnnotation(t *testing.T) {
	cfg := testCfg()
	cm := ServiceCAConfigMap(cfg)
	if cm.Name != NameServiceCAConfigMap(cfg) {
		t.Errorf("Name = %q", cm.Name)
	}
	want := "service.beta.openshift.io/inject-cabundle"
	if cm.Annotations[want] != "true" {
		t.Errorf("annotations = %v, want %s=true", cm.Annotations, want)
	}
}
