package advancedcronjob

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/klog/v2"

	appsv1beta1 "github.com/openkruise/kruise/apis/apps/v1beta1"
)

func FindTemplateKind(spec appsv1beta1.AdvancedCronJobSpec) appsv1beta1.TemplateKind {
	if spec.Template.JobTemplate != nil {
		return appsv1beta1.JobTemplate
	}

	if spec.Template.ImageListPullJobTemplate != nil {
		return appsv1beta1.ImageListPullJobTemplate
	}

	return appsv1beta1.BroadcastJobTemplate
}

func formatSchedule(acj *appsv1beta1.AdvancedCronJob) string {
	if strings.Contains(acj.Spec.Schedule, "TZ") {
		return acj.Spec.Schedule
	}
	if acj.Spec.TimeZone != nil {
		if _, err := time.LoadLocation(*acj.Spec.TimeZone); err != nil {
			klog.ErrorS(err, "Failed to load location for advancedCronJob", "location", *acj.Spec.TimeZone, "advancedCronJob", klog.KObj(acj))
			return acj.Spec.Schedule
		}
		return fmt.Sprintf("TZ=%s %s", *acj.Spec.TimeZone, acj.Spec.Schedule)
	}
	return acj.Spec.Schedule
}
