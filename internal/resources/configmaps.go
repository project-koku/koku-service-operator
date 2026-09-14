package resources

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costv1alpha1 "github.com/project-koku/koku-service-operator/api/v1alpha1"
)

// DBInitConfigMap builds the ConfigMap containing the PostgreSQL init script
// that creates per-service databases and users.
func DBInitConfigMap(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameDBInitConfigMap(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "database"),
		},
		Data: map[string]string{
			"init-databases.sh": dbInitScript(),
		},
	}
}

func dbInitScript() string {
	return `#!/bin/bash
set -e
echo "Initializing databases with user-specific credentials..."

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres <<EOF
DO \$\$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = '$ROS_USER') THEN
    EXECUTE 'CREATE USER ' || quote_ident('$ROS_USER') || ' WITH PASSWORD ' || quote_literal('$ROS_PASSWORD');
  END IF;
  IF NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = '$KRUIZE_USER') THEN
    EXECUTE 'CREATE USER ' || quote_ident('$KRUIZE_USER') || ' WITH PASSWORD ' || quote_literal('$KRUIZE_PASSWORD');
  END IF;
  IF NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = '$RBAC_USER') THEN
    EXECUTE 'CREATE USER ' || quote_ident('$RBAC_USER') || ' WITH PASSWORD ' || quote_literal('$RBAC_PASSWORD');
  END IF;
  IF NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname = '$KOKU_USER') THEN
    EXECUTE 'CREATE USER ' || quote_ident('$KOKU_USER') || ' WITH PASSWORD ' || quote_literal('$KOKU_PASSWORD');
  END IF;
END
\$\$;

SELECT 'CREATE DATABASE costonprem_ros'   WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'costonprem_ros')\gexec
SELECT 'CREATE DATABASE costonprem_kruize' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'costonprem_kruize')\gexec
SELECT 'CREATE DATABASE costonprem_koku'  WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'costonprem_koku')\gexec
SELECT 'CREATE DATABASE costonprem_rbac'  WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'costonprem_rbac')\gexec

GRANT ALL PRIVILEGES ON DATABASE costonprem_ros    TO $ROS_USER;    ALTER DATABASE costonprem_ros    OWNER TO $ROS_USER;
GRANT ALL PRIVILEGES ON DATABASE costonprem_kruize TO $KRUIZE_USER; ALTER DATABASE costonprem_kruize OWNER TO $KRUIZE_USER;
GRANT ALL PRIVILEGES ON DATABASE costonprem_rbac   TO $RBAC_USER;   ALTER DATABASE costonprem_rbac   OWNER TO $RBAC_USER;
GRANT ALL PRIVILEGES ON DATABASE costonprem_koku   TO $KOKU_USER;   ALTER DATABASE costonprem_koku   OWNER TO $KOKU_USER;

\c costonprem_koku
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
EOF
`
}

// AWSConfigMap builds the ConfigMap used by boto3 to configure S3 addressing.
func AWSConfigMap(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.ConfigMap {
	style := cfg.Spec.ObjectStorage.S3.AddressingStyle
	if style == "" {
		style = "path"
	}
	region := S3Region(cfg)
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameAWSConfigMap(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "cost-management"),
		},
		Data: map[string]string{
			"config": "[default]\nregion = " + region + "\ns3 =\n  signature_version = s3v4\n  addressing_style = " + style + "\n",
		},
	}
}

// CACombineConfigMap builds the ConfigMap with the combine-ca.sh helper script.
func CACombineConfigMap(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameCACombineConfigMap(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "cost-management"),
		},
		Data: map[string]string{
			"combine-ca.sh": `#!/bin/bash
set -e

if [ -f /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem ]; then
  cat /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem > /ca-output/ca-bundle.crt
elif [ -f /etc/ssl/certs/ca-bundle.crt ]; then
  cat /etc/ssl/certs/ca-bundle.crt > /ca-output/ca-bundle.crt
elif [ -f /etc/ssl/certs/ca-certificates.crt ]; then
  cat /etc/ssl/certs/ca-certificates.crt > /ca-output/ca-bundle.crt
else
  touch /ca-output/ca-bundle.crt
fi

if [ -f /var/run/secrets/kubernetes.io/serviceaccount/ca.crt ]; then
  echo "" >> /ca-output/ca-bundle.crt
  cat /var/run/secrets/kubernetes.io/serviceaccount/ca.crt >> /ca-output/ca-bundle.crt
fi

if [ -f /ca-source/service-ca.crt ] && [ -s /ca-source/service-ca.crt ]; then
  echo "" >> /ca-output/ca-bundle.crt
  cat /ca-source/service-ca.crt >> /ca-output/ca-bundle.crt
fi

for cert in /ca-extra/*.crt; do
  [ -f "$cert" ] && [ -s "$cert" ] || continue
  echo "" >> /ca-output/ca-bundle.crt
  cat "$cert" >> /ca-output/ca-bundle.crt
done

echo "CA bundle combined: $(grep -c 'BEGIN CERTIFICATE' /ca-output/ca-bundle.crt 2>/dev/null || echo 0) certificates"
`,
		},
	}
}

// ServiceCAConfigMap creates a placeholder ConfigMap for the cluster service CA.
// On OpenShift, this is populated by the service-ca-operator via an annotation.
func ServiceCAConfigMap(cfg *costv1alpha1.CostManagementServiceConfig) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      NameServiceCAConfigMap(cfg),
			Namespace: cfg.Namespace,
			Labels:    Labels(cfg, "cost-management"),
			Annotations: map[string]string{
				// OpenShift service-ca-operator injects the cluster CA into this key.
				"service.beta.openshift.io/inject-cabundle": "true",
			},
		},
		Data: map[string]string{},
	}
}
