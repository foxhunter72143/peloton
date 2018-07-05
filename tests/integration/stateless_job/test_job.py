import pytest

from tests.integration.job import IntegrationTestConfig, Job

pytestmark = [pytest.mark.default, pytest.mark.stateless]


def test__create_job(stateless_job):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state='RUNNING')


def test__stop_stateless_job(stateless_job):
    stateless_job.create()
    stateless_job.wait_for_state(goal_state='RUNNING')
    stateless_job.stop()
    stateless_job.wait_for_state(goal_state='KILLED')


def test__exit_task_automatically_restart():
    job = Job(job_file='test_stateless_job_exit_without_err.yaml',
              config=IntegrationTestConfig(max_retry_attempts=100))
    job.create()
    job.wait_for_state(goal_state='RUNNING')

    def job_not_running():
        return job.get_runtime().state != 'RUNNING'

    job.wait_for_condition(job_not_running)
    job.wait_for_state(goal_state='RUNNING')


def test__failed_task_automatically_restart():
    job = Job(job_file='test_stateless_job_exit_with_err.yaml',
              config=IntegrationTestConfig(max_retry_attempts=100))
    job.create()
    job.wait_for_state(goal_state='RUNNING')

    def job_not_running():
        return job.get_runtime().state != 'RUNNING'

    job.wait_for_condition(job_not_running)
    job.wait_for_state(goal_state='RUNNING')