package cmds

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/golang/protobuf/jsonpb"
	"github.com/pachyderm/pachyderm/src/pps"
	"github.com/pachyderm/pachyderm/src/pps/example"
	"github.com/pachyderm/pachyderm/src/pps/pretty"
	"github.com/spf13/cobra"
	"go.pedge.io/pkg/cobra"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

func Cmds(address string) ([]*cobra.Command, error) {
	marshaller := &jsonpb.Marshaler{Indent: "  "}

	exampleCreateJobRequest, err := marshaller.MarshalToString(example.CreateJobRequest())
	if err != nil {
		return nil, err
	}
	var jobPath string
	createJob := &cobra.Command{
		Use:   "create-job -f job.json",
		Short: "Create a new job. Returns the id of the created job.",
		Long:  fmt.Sprintf("Create a new job from a spec, the spec looks like this\n%s", exampleCreateJobRequest),
		Run: func(cmd *cobra.Command, args []string) {
			apiClient, err := getAPIClient(address)
			if err != nil {
				errorAndExit("Error connecting to pps: %s", err.Error())
			}
			var jobReader io.Reader
			if jobPath == "-" {
				jobReader = os.Stdin
				fmt.Print("Reading from stdin.\n")
			} else {
				jobFile, err := os.Open(jobPath)
				if err != nil {
					errorAndExit("Error opening %s: %s", jobPath, err.Error())
				}
				defer func() {
					if err := jobFile.Close(); err != nil {
						errorAndExit("Error closing%s: %s", jobPath, err.Error())
					}
				}()
				jobReader = jobFile
			}
			var request pps.CreateJobRequest
			if err := jsonpb.Unmarshal(jobReader, &request); err != nil {
				errorAndExit("Error reading from stdin: %s", err.Error())
			}
			job, err := apiClient.CreateJob(
				context.Background(),
				&request,
			)
			if err != nil {
				errorAndExit("Error from CreateJob: %s", err.Error())
			}
			fmt.Println(job.Id)
		},
	}
	createJob.Flags().StringVarP(&jobPath, "file", "f", "-", "The file containing the job, - reads from stdin.")

	inspectJob := &cobra.Command{
		Use:   "inspect-job job-id",
		Short: "Return info about a job.",
		Long:  "Return info about a job.",
		Run: pkgcobra.RunFixedArgs(1, func(args []string) error {
			apiClient, err := getAPIClient(address)
			if err != nil {
				return err
			}
			jobInfo, err := apiClient.InspectJob(
				context.Background(),
				&pps.InspectJobRequest{
					Job: &pps.Job{
						Id: args[0],
					},
				},
			)
			if err != nil {
				errorAndExit("Error from InspectJob: %s", err.Error())
			}
			if jobInfo == nil {
				errorAndExit("Job %s not found.", args[0])
			}
			writer := tabwriter.NewWriter(os.Stdout, 20, 1, 3, ' ', 0)
			pretty.PrintJobHeader(writer)
			pretty.PrintJobInfo(writer, jobInfo)
			return writer.Flush()
		}),
	}

	var pipelineName string
	listJob := &cobra.Command{
		Use:   "list-job -p pipeline-name",
		Short: "Return info about all jobs.",
		Long:  "Return info about all jobs.",
		Run: pkgcobra.RunFixedArgs(0, func(args []string) error {
			apiClient, err := getAPIClient(address)
			if err != nil {
				return err
			}
			var pipeline *pps.Pipeline
			if pipelineName != "" {
				pipeline = &pps.Pipeline{
					Name: pipelineName,
				}
			}
			jobInfos, err := apiClient.ListJob(
				context.Background(),
				&pps.ListJobRequest{
					Pipeline: pipeline,
				},
			)
			if err != nil {
				errorAndExit("Error from InspectJob: %s", err.Error())
			}
			writer := tabwriter.NewWriter(os.Stdout, 20, 1, 3, ' ', 0)
			pretty.PrintJobHeader(writer)
			for _, jobInfo := range jobInfos.JobInfo {
				pretty.PrintJobInfo(writer, jobInfo)
			}
			return writer.Flush()
		}),
	}
	listJob.Flags().StringVarP(&pipelineName, "pipeline", "p", "", "Limit to jobs made by pipeline.")

	var pipelinePath string
	exampleCreatePipelineRequest, err := marshaller.MarshalToString(example.CreatePipelineRequest())
	if err != nil {
		return nil, err
	}
	createPipeline := &cobra.Command{
		Use:   "create-pipeline -f pipeline.json",
		Short: "Create a new pipeline.",
		Long:  fmt.Sprintf("Create a new pipeline from a spec, the spec looks like this\n%s", exampleCreatePipelineRequest),
		Run: func(cmd *cobra.Command, args []string) {
			apiClient, err := getAPIClient(address)
			if err != nil {
				errorAndExit("Error connecting to pps: %s", err.Error())
			}
			var pipelineReader io.Reader
			if pipelinePath == "-" {
				pipelineReader = os.Stdin
				fmt.Print("Reading from stdin.\n")
			} else {
				pipelineFile, err := os.Open(pipelinePath)
				if err != nil {
					errorAndExit("Error opening %s: %s", pipelinePath, err.Error())
				}
				defer func() {
					if err := pipelineFile.Close(); err != nil {
						errorAndExit("Error closing%s: %s", pipelinePath, err.Error())
					}
				}()
				pipelineReader = pipelineFile
			}
			var request pps.CreatePipelineRequest
			if err := jsonpb.Unmarshal(pipelineReader, &request); err != nil {
				errorAndExit("Error reading from stdin: %s", err.Error())
			}
			if _, err := apiClient.CreatePipeline(
				context.Background(),
				&request,
			); err != nil {
				errorAndExit("Error from CreatePipeline: %s", err.Error())
			}
		},
	}
	createPipeline.Flags().StringVarP(&pipelinePath, "file", "f", "-", "The file containing the pipeline, - reads from stdin.")

	inspectPipeline := &cobra.Command{
		Use:   "inspect-pipeline pipeline-name",
		Short: "Return info about a pipeline.",
		Long:  "Return info about a pipeline.",
		Run: pkgcobra.RunFixedArgs(1, func(args []string) error {
			apiClient, err := getAPIClient(address)
			if err != nil {
				return err
			}
			pipelineInfo, err := apiClient.InspectPipeline(
				context.Background(),
				&pps.InspectPipelineRequest{
					Pipeline: &pps.Pipeline{
						Name: args[0],
					},
				},
			)
			if err != nil {
				errorAndExit("Error from InspectPipeline: %s", err.Error())
			}
			if pipelineInfo == nil {
				errorAndExit("Pipeline %s not found.", args[0])
			}
			writer := tabwriter.NewWriter(os.Stdout, 20, 1, 3, ' ', 0)
			pretty.PrintPipelineHeader(writer)
			pretty.PrintPipelineInfo(writer, pipelineInfo)
			return writer.Flush()
		}),
	}

	listPipeline := &cobra.Command{
		Use:   "list-pipeline",
		Short: "Return info about all pipelines.",
		Long:  "Return info about all pipelines.",
		Run: pkgcobra.RunFixedArgs(0, func(args []string) error {
			apiClient, err := getAPIClient(address)
			if err != nil {
				return err
			}
			pipelineInfos, err := apiClient.ListPipeline(
				context.Background(),
				&pps.ListPipelineRequest{},
			)
			if err != nil {
				errorAndExit("Error from ListPipeline: %s", err.Error())
			}
			writer := tabwriter.NewWriter(os.Stdout, 20, 1, 3, ' ', 0)
			pretty.PrintPipelineHeader(writer)
			for _, pipelineInfo := range pipelineInfos.PipelineInfo {
				pretty.PrintPipelineInfo(writer, pipelineInfo)
			}
			return writer.Flush()
		}),
	}

	deletePipeline := &cobra.Command{
		Use:   "delete-pipeline pipeline-name",
		Short: "Delete a pipeline.",
		Long:  "Delete a pipeline.",
		Run: pkgcobra.RunFixedArgs(1, func(args []string) error {
			apiClient, err := getAPIClient(address)
			if err != nil {
				return err
			}
			if _, err := apiClient.DeletePipeline(
				context.Background(),
				&pps.DeletePipelineRequest{
					Pipeline: &pps.Pipeline{
						Name: args[0],
					},
				},
			); err != nil {
				errorAndExit("Error from DeletePipeline: %s", err.Error())
			}
			return nil
		}),
	}

	var result []*cobra.Command
	result = append(result, createJob)
	result = append(result, inspectJob)
	result = append(result, listJob)
	result = append(result, createPipeline)
	result = append(result, inspectPipeline)
	result = append(result, listPipeline)
	result = append(result, deletePipeline)
	return result, nil
}

func errorAndExit(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "%s\n", fmt.Sprintf(format, args...))
	os.Exit(1)
}

func getAPIClient(address string) (pps.APIClient, error) {
	clientConn, err := grpc.Dial(address, grpc.WithInsecure())
	if err != nil {
		return nil, err
	}
	return pps.NewAPIClient(clientConn), nil
}
