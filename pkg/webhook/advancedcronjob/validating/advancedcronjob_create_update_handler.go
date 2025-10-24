/*
Copyright 2020 The Kruise Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package validating

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	admissionv1 "k8s.io/api/admission/v1"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	genericvalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	validationutil "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/kubernetes/pkg/apis/core"
	corev1 "k8s.io/kubernetes/pkg/apis/core/v1"
	apivalidation "k8s.io/kubernetes/pkg/apis/core/validation"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	appsv1alpha1 "github.com/openkruise/kruise/apis/apps/v1alpha1"
	appsv1beta1 "github.com/openkruise/kruise/apis/apps/v1beta1"
	daemonutil "github.com/openkruise/kruise/pkg/daemon/util"
	webhookutil "github.com/openkruise/kruise/pkg/webhook/util"
)

const (
	AdvancedCronJobNameMaxLen      = 63
	validateAdvancedCronJobNameMsg = "AdvancedCronJob name must consist of alphanumeric characters or '-'"
	validAdvancedCronJobNameFmt    = `^[a-zA-Z0-9\-]+$`
	MaxActiveDeadLineSeconds       = 3600 * 24
	MaxTTLSecondsAfterFinished     = 3600 * 24 * 3
)

var (
	validateAdvancedCronJobNameRegex = regexp.MustCompile(validAdvancedCronJobNameFmt)
)

// AdvancedCronJobCreateUpdateHandler handles AdvancedCronJob
type AdvancedCronJobCreateUpdateHandler struct {
	// Decoder decodes objects
	Decoder admission.Decoder
}

func (h *AdvancedCronJobCreateUpdateHandler) validateAdvancedCronJob(obj *appsv1beta1.AdvancedCronJob) field.ErrorList {
	allErrs := genericvalidation.ValidateObjectMeta(&obj.ObjectMeta, true, validateAdvancedCronJobName, field.NewPath("metadata"))
	allErrs = append(allErrs, validateAdvancedCronJobSpec(&obj.Spec, field.NewPath("spec"))...)
	return allErrs
}

func validateAdvancedCronJobSpec(spec *appsv1beta1.AdvancedCronJobSpec, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, validateAdvancedCronJobSpecSchedule(spec, fldPath)...)
	allErrs = append(allErrs, validateAdvancedCronJobSpecTemplate(spec, fldPath)...)
	if spec.StartingDeadlineSeconds != nil {
		allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(*spec.StartingDeadlineSeconds, fldPath.Child("startingDeadlineSeconds"))...)
	}
	if spec.SuccessfulJobsHistoryLimit != nil {
		allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(*spec.SuccessfulJobsHistoryLimit), fldPath.Child("successfulJobsHistoryLimit"))...)
	}
	if spec.FailedJobsHistoryLimit != nil {
		allErrs = append(allErrs, apivalidation.ValidateNonnegativeField(int64(*spec.FailedJobsHistoryLimit), fldPath.Child("failedJobsHistoryLimit"))...)
	}
	allErrs = append(allErrs, validateTimeZone(spec.TimeZone, fldPath.Child("timeZone"))...)
	return allErrs
}

func validateAdvancedCronJobSpecSchedule(spec *appsv1beta1.AdvancedCronJobSpec, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if len(spec.Schedule) == 0 {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("schedule"),
			spec.Schedule,
			"schedule cannot be empty, please provide valid cron schedule."))
	}

	// Use a helper function to safely parse cron expressions and handle panics
	if err := validateCronSchedule(spec.Schedule); err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("schedule"),
			spec.Schedule, err.Error()))
	}
	if strings.Contains(spec.Schedule, "TZ") && spec.TimeZone != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("schedule"),
			spec.Schedule, "cannot use both timeZone field and TZ or CRON_TZ in schedule"))
	}
	return allErrs
}

// validateCronSchedule safely validates a cron schedule expression, handling potential panics
func validateCronSchedule(schedule string) error {
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				// If panic occurs, convert it to an error
				err = fmt.Errorf("invalid cron schedule: %v", r)
			}
		}()

		_, parseErr := cron.ParseStandard(schedule)
		err = parseErr
	}()

	return err
}

func validateTimeZone(timeZone *string, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if timeZone == nil {
		return allErrs
	}

	if len(*timeZone) == 0 {
		allErrs = append(allErrs, field.Invalid(fldPath, timeZone, "timeZone must be nil or non-empty string"))
		return allErrs
	}

	if strings.EqualFold(*timeZone, "Local") {
		allErrs = append(allErrs, field.Invalid(fldPath, timeZone, "timeZone must be an explicit time zone as defined in https://www.iana.org/time-zones"))
	}

	if _, err := time.LoadLocation(*timeZone); err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath, timeZone, err.Error()))
	}

	return allErrs
}

func validateAdvancedCronJobSpecTemplate(spec *appsv1beta1.AdvancedCronJobSpec, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	templateCount := 0
	if spec.Template.JobTemplate != nil {
		templateCount++
		allErrs = append(allErrs, validateJobTemplateSpec(spec.Template.JobTemplate, fldPath)...)
	}

	if spec.Template.BroadcastJobTemplate != nil {
		templateCount++
		allErrs = append(allErrs, validateBroadcastJobTemplateSpec(spec.Template.BroadcastJobTemplate, fldPath)...)
	}

	if spec.Template.ImageListPullJobTemplate != nil {
		templateCount++
		switch spec.ConcurrencyPolicy {
		case appsv1beta1.ReplaceConcurrent, appsv1beta1.ForbidConcurrent:
		default:
			allErrs = append(allErrs, field.Invalid(fldPath.Child("spec").Child("concurrencyPolicy"), spec.ConcurrencyPolicy, fmt.Sprintf("concurrencyPolicy should be Replace or Forbid, but current value is: %s", spec.ConcurrencyPolicy)))
		}
		allErrs = append(allErrs, validateImageListPullJobTemplateSpec(spec.Template.ImageListPullJobTemplate, fldPath.Child("template").Child("imageListPullJobTemplate"))...)
	}

	if templateCount == 0 {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("template"),
			"spec must have one template, either JobTemplate or BroadcastJobTemplate or ImageListPullJobTemplate should be provided"))
	} else if templateCount > 1 {
		allErrs = append(allErrs, field.Forbidden(fldPath.Child("template"),
			"spec can have only one template, either JobTemplate or BroadcastJobTemplate or ImageListPullJobTemplate should be provided"))
	}
	return allErrs
}

func validateJobTemplateSpec(jobSpec *batchv1.JobTemplateSpec, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	coreTemplate, err := convertPodTemplateSpec(&jobSpec.Spec.Template)
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Root(), jobSpec.Spec.Template, fmt.Sprintf("Convert_v1_PodTemplateSpec_To_core_PodTemplateSpec failed: %v", err)))
		return allErrs
	}
	return append(allErrs, apivalidation.ValidatePodTemplateSpec(coreTemplate, fldPath.Child("template"), webhookutil.DefaultPodValidationOptions)...)
}

func validateBroadcastJobTemplateSpec(brJobSpec *appsv1beta1.BroadcastJobTemplateSpec, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	coreTemplate, err := convertPodTemplateSpec(&brJobSpec.Spec.Template)
	if err != nil {
		allErrs = append(allErrs, field.Invalid(fldPath.Root(), brJobSpec.Spec.Template, fmt.Sprintf("Convert_v1_PodTemplateSpec_To_core_PodTemplateSpec failed: %v", err)))
		return allErrs
	}
	return append(allErrs, apivalidation.ValidatePodTemplateSpec(coreTemplate, fldPath.Child("template"), webhookutil.DefaultPodValidationOptions)...)
}

func validateImageListPullJobTemplateSpec(ilpJobSpec *appsv1beta1.ImageListPullJobTemplateSpec, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if ilpJobSpec.Spec.Selector != nil {
		if ilpJobSpec.Spec.Selector.MatchLabels != nil || ilpJobSpec.Spec.Selector.MatchExpressions != nil {
			if ilpJobSpec.Spec.Selector.Names != nil {
				return append(allErrs, field.Invalid(fldPath.Child("spec").Child("selector"), ilpJobSpec.Spec.Selector, "can not set both names and labelSelector in this spec.selector"))
			}
			if _, err := metav1.LabelSelectorAsSelector(&ilpJobSpec.Spec.Selector.LabelSelector); err != nil {
				return append(allErrs, field.Invalid(fldPath.Child("spec").Child("selector").Child("labelSelector"), ilpJobSpec.Spec.Selector.LabelSelector, fmt.Sprintf("invalid selector: %v", err)))
			}
		}
		if ilpJobSpec.Spec.Selector.Names != nil {
			names := sets.NewString(ilpJobSpec.Spec.Selector.Names...)
			if names.Len() != len(ilpJobSpec.Spec.Selector.Names) {
				return append(allErrs, field.Invalid(fldPath.Child("spec").Child("selector").Child("names"), ilpJobSpec.Spec.Selector.Names, "duplicated name in selector names"))
			}
		}
	}

	if ilpJobSpec.Spec.PodSelector != nil {
		if ilpJobSpec.Spec.Selector != nil {
			return append(allErrs, field.Invalid(fldPath.Child("spec"), ilpJobSpec.Spec, "can not set both selector and podSelector"))
		}
		if _, err := metav1.LabelSelectorAsSelector(&ilpJobSpec.Spec.PodSelector.LabelSelector); err != nil {
			return append(allErrs, field.Invalid(fldPath.Child("spec").Child("podSelector").Child("labelSelector"), ilpJobSpec.Spec.PodSelector.LabelSelector, fmt.Sprintf("invalid selector: %v", err)))
		}
	}

	if len(ilpJobSpec.Spec.Images) == 0 {
		return append(allErrs, field.Invalid(fldPath.Child("spec").Child("images"), ilpJobSpec.Spec.Images, "image can not be empty"))
	}

	if len(ilpJobSpec.Spec.Images) > 255 {
		return append(allErrs, field.Invalid(fldPath.Child("spec").Child("images"), ilpJobSpec.Spec.Images, "the maximum number of images cannot > 255"))
	}

	for i := 0; i < len(ilpJobSpec.Spec.Images); i++ {
		for j := i + 1; j < len(ilpJobSpec.Spec.Images); j++ {
			if ilpJobSpec.Spec.Images[i] == ilpJobSpec.Spec.Images[j] {
				return append(allErrs, field.Invalid(fldPath.Child("spec").Child("images"), ilpJobSpec.Spec.Images, "images cannot have duplicate values"))
			}
		}
	}

	for _, image := range ilpJobSpec.Spec.Images {
		if _, err := daemonutil.NormalizeImageRef(image); err != nil {
			return append(allErrs, field.Invalid(fldPath.Child("spec").Child("images"), ilpJobSpec.Spec.Images, fmt.Sprintf("invalid image %s: %v", image, err)))
		}
	}

	switch ilpJobSpec.Spec.CompletionPolicy.Type {
	case appsv1beta1.Always:
		// is a no-op here. No need to do parameter dependency verification in this type.
		if ilpJobSpec.Spec.CompletionPolicy.ActiveDeadlineSeconds != nil && *ilpJobSpec.Spec.CompletionPolicy.ActiveDeadlineSeconds > MaxActiveDeadLineSeconds {
			return append(allErrs, field.Invalid(fldPath.Child("spec").Child("completionPolicy").Child("activeDeadlineSeconds"), ilpJobSpec.Spec.CompletionPolicy.ActiveDeadlineSeconds, fmt.Sprintf("activeDeadlineSeconds must be less than %d, current value is: %d", MaxActiveDeadLineSeconds, *ilpJobSpec.Spec.CompletionPolicy.ActiveDeadlineSeconds)))
		}
		if ilpJobSpec.Spec.CompletionPolicy.ActiveDeadlineSeconds != nil && ilpJobSpec.Spec.PullPolicy != nil && ilpJobSpec.Spec.PullPolicy.TimeoutSeconds != nil && int64(*ilpJobSpec.Spec.PullPolicy.TimeoutSeconds) > *ilpJobSpec.Spec.CompletionPolicy.ActiveDeadlineSeconds {
			return append(allErrs, field.Invalid(fldPath.Child("spec").Child("completionPolicy").Child("activeDeadlineSeconds"), ilpJobSpec.Spec.CompletionPolicy.ActiveDeadlineSeconds, fmt.Sprintf("completionPolicy.activeDeadlineSeconds must be greater than pullPolicy.timeoutSeconds(%d)", *ilpJobSpec.Spec.PullPolicy.TimeoutSeconds)))
		}
		if ilpJobSpec.Spec.CompletionPolicy.TTLSecondsAfterFinished != nil && *ilpJobSpec.Spec.CompletionPolicy.TTLSecondsAfterFinished > MaxTTLSecondsAfterFinished {
			return append(allErrs, field.Invalid(fldPath.Child("spec").Child("completionPolicy").Child("ttlSecondsAfterFinished"), ilpJobSpec.Spec.CompletionPolicy.TTLSecondsAfterFinished, fmt.Sprintf("ttlSecondsAfterFinished must be less than %d, current value is: %d", MaxTTLSecondsAfterFinished, *ilpJobSpec.Spec.CompletionPolicy.TTLSecondsAfterFinished)))
		}
	default:
		return append(allErrs, field.Invalid(fldPath.Child("spec").Child("completionPolicy").Child("type"), ilpJobSpec.Spec.CompletionPolicy.Type, fmt.Sprintf("completionPolicy should be Always, but current value is: %s", ilpJobSpec.Spec.CompletionPolicy.Type)))
	}

	return allErrs
}

func convertPodTemplateSpec(template *v1.PodTemplateSpec) (*core.PodTemplateSpec, error) {
	coreTemplate := &core.PodTemplateSpec{}
	if err := corev1.Convert_v1_PodTemplateSpec_To_core_PodTemplateSpec(template.DeepCopy(), coreTemplate, nil); err != nil {
		return nil, err
	}
	return coreTemplate, nil
}

func validateAdvancedCronJobName(name string, prefix bool) (allErrs []string) {
	if !validateAdvancedCronJobNameRegex.MatchString(name) {
		allErrs = append(allErrs, validationutil.RegexError(validateAdvancedCronJobNameMsg, validAdvancedCronJobNameFmt, "example-com"))
	}
	if len(name) > AdvancedCronJobNameMaxLen {
		allErrs = append(allErrs, validationutil.MaxLenError(AdvancedCronJobNameMaxLen))
	}
	return allErrs
}

func (h *AdvancedCronJobCreateUpdateHandler) validateAdvancedCronJobUpdate(obj, oldObj *appsv1beta1.AdvancedCronJob) field.ErrorList {
	allErrs := apivalidation.ValidateObjectMetaUpdate(&obj.ObjectMeta, &oldObj.ObjectMeta, field.NewPath("metadata"))
	allErrs = append(allErrs, validateAdvancedCronJobSpec(&obj.Spec, field.NewPath("spec"))...)

	advanceCronJob := obj.DeepCopy()
	advanceCronJob.Spec.Schedule = oldObj.Spec.Schedule
	advanceCronJob.Spec.ConcurrencyPolicy = oldObj.Spec.ConcurrencyPolicy
	advanceCronJob.Spec.SuccessfulJobsHistoryLimit = oldObj.Spec.SuccessfulJobsHistoryLimit
	advanceCronJob.Spec.FailedJobsHistoryLimit = oldObj.Spec.FailedJobsHistoryLimit
	advanceCronJob.Spec.StartingDeadlineSeconds = oldObj.Spec.StartingDeadlineSeconds
	advanceCronJob.Spec.Paused = oldObj.Spec.Paused
	advanceCronJob.Spec.TimeZone = oldObj.Spec.TimeZone
	if oldObj.Spec.Template.ImageListPullJobTemplate != nil {
		advanceCronJob.Spec.Template.ImageListPullJobTemplate = oldObj.Spec.Template.ImageListPullJobTemplate
	}
	if !apiequality.Semantic.DeepEqual(advanceCronJob.Spec, oldObj.Spec) {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec"), "updates to advancedcronjob spec for fields other than 'imageListPullJobTemplate', 'schedule', 'concurrencyPolicy', 'successfulJobsHistoryLimit', 'failedJobsHistoryLimit', 'startingDeadlineSeconds', 'timeZone' and 'paused' are forbidden"))
	}
	return allErrs
}

func (h *AdvancedCronJobCreateUpdateHandler) decodeAdvancedCronJob(req admission.Request, obj *appsv1beta1.AdvancedCronJob) error {
	switch req.AdmissionRequest.Resource.Version {
	case appsv1beta1.GroupVersion.Version:
		if err := h.Decoder.Decode(req, obj); err != nil {
			return err
		}
	case appsv1alpha1.GroupVersion.Version:
		objv1alpha1 := &appsv1alpha1.AdvancedCronJob{}
		if err := h.Decoder.Decode(req, objv1alpha1); err != nil {
			return err
		}
		if err := objv1alpha1.ConvertTo(obj); err != nil {
			return fmt.Errorf("failed to convert v1alpha1->v1beta1: %v", err)
		}
	}
	return nil
}

func (h *AdvancedCronJobCreateUpdateHandler) decodeAdvancedCronJobFromRaw(raw runtime.RawExtension, version string, obj *appsv1beta1.AdvancedCronJob) error {
	switch version {
	case appsv1beta1.GroupVersion.Version:
		if err := h.Decoder.DecodeRaw(raw, obj); err != nil {
			return err
		}
	case appsv1alpha1.GroupVersion.Version:
		objv1alpha1 := &appsv1alpha1.AdvancedCronJob{}
		if err := h.Decoder.DecodeRaw(raw, objv1alpha1); err != nil {
			return err
		}
		if err := objv1alpha1.ConvertTo(obj); err != nil {
			return fmt.Errorf("failed to convert v1alpha1->v1beta1: %v", err)
		}
	default:
		return fmt.Errorf("unsupported version: %s", version)
	}
	return nil
}

var _ admission.Handler = &AdvancedCronJobCreateUpdateHandler{}

// Handle handles admission requests.
func (h *AdvancedCronJobCreateUpdateHandler) Handle(ctx context.Context, req admission.Request) admission.Response {
	obj := &appsv1beta1.AdvancedCronJob{}

	err := h.decodeAdvancedCronJob(req, obj)
	if err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	switch req.AdmissionRequest.Operation {
	case admissionv1.Create:
		if allErrs := h.validateAdvancedCronJob(obj); len(allErrs) > 0 {
			return admission.Errored(http.StatusUnprocessableEntity, allErrs.ToAggregate())
		}
	case admissionv1.Update:
		oldObj := &appsv1beta1.AdvancedCronJob{}
		if err := h.decodeAdvancedCronJobFromRaw(req.AdmissionRequest.OldObject, req.AdmissionRequest.Resource.Version, oldObj); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}

		if allErrs := h.validateAdvancedCronJobUpdate(obj, oldObj); len(allErrs) > 0 {
			return admission.Errored(http.StatusUnprocessableEntity, allErrs.ToAggregate())
		}
	}

	return admission.ValidationResponse(true, "")
}
