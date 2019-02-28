import time

from tests.integration.aurorabridge_test.client import api
from tests.integration.util import load_config

TEST_CONFIG_DIR = '/aurorabridge_test/test_configs'


def check_response_ok(response):
    '''Asserts Aurora's response code is "OK".

    Args:
        response: aurora response
    '''

    assert response.responseCode == api.ResponseCode.OK, \
        'bad response: {code} {msg}'.format(
            code=api.ResponseCode.name_of(response.responseCode),
            msg=','.join(map(lambda d: d.message, response.details)))


def wait_for_rolled_forward(client, job_update_key):
    '''Wait for job update to be in "ROLLED_FORWARD" state, triggers
    assertion failure if timed out.

    Args:
        client: aurora client object
        job_update_key: aurora JobUpdateKey struct specifying the update to
            wait for
    '''
    wait_for_update_status(
        client,
        job_update_key,
        {api.JobUpdateStatus.ROLLING_FORWARD},
        api.JobUpdateStatus.ROLLED_FORWARD)


def wait_for_update_status(
        client,
        job_update_key,
        allowed_intermediate_statuses,
        status,
        timeout_secs=240):
    '''Wait for job update to be in specific state, triggers assertion
    failure if timed out.

    Args:
        client: aurora client object
        job_update_key: aurora JobUpdateKey struct specifying the update to
            wait for
        allowed_intermediate_statuses: a list of intermediate update state
            allowed to be in, fails immediately if update is neither in
            intermediate state or target state
        status: target update state to wait for
        timeout_secs: timeout in seconds, triggers assertion failure if
            timed out
    '''

    deadline = time.time() + timeout_secs
    while time.time() < deadline:
        latest = get_update_status(client, job_update_key)
        if latest == status:
            return
        assert latest in allowed_intermediate_statuses
        time.sleep(2)

    assert False, 'timed out waiting for {status}, last status: {latest}'.format(
        status=api.JobUpdateStatus.name_of(status),
        latest=api.JobUpdateStatus.name_of(latest))


def get_update_status(client, job_update_key):
    '''Querying current job update status.

    Args:
        client: aurora client object
        job_update_key: aurora JobUpdateKey struct specifying the update to
            query for

    Returns:
        aurora JobUpdateStatus enum
    '''
    res = client.get_job_update_summaries(api.JobUpdateQuery(key=job_update_key))
    check_response_ok(res)

    summaries = res.result.getJobUpdateSummariesResult.updateSummaries
    assert summaries is not None and len(summaries) == 1

    summary = summaries[0]
    assert summary.key == job_update_key

    return summary.state.status


def wait_for_running(client, job_key):
    '''Wait for all tasks in a specific job to be in "RUNNING" state, triggers
    assertion failure if timed out.

    Args:
        client: aurora client object
        job_key: aurora JobKey struct specifying the job to wait for
    '''
    wait_for_task_status(
        client,
        job_key,
        set([
            api.ScheduleStatus.INIT,
            api.ScheduleStatus.PENDING,
            api.ScheduleStatus.ASSIGNED,
            api.ScheduleStatus.STARTING,
        ]),
        api.ScheduleStatus.RUNNING)


def wait_for_killed(client, job_key):
    '''Wait for all tasks in a specific job to be in "KILLED" state, triggers
    assertion failure if timed out.

    Args:
        client: aurora client object
        job_key: aurora JobKey struct specifying the job to wait for
    '''
    wait_for_task_status(
        client,
        job_key,
        set([
            api.ScheduleStatus.RUNNING,
            api.ScheduleStatus.KILLING,
        ]),
        api.ScheduleStatus.KILLED)


def wait_for_task_status(
        client,
        job_key,
        allowed_intermediate_statuses,
        status,
        timeout_secs=240):
    '''Wait for all tasks in a job to be in specific state, triggers assertion
    failure if timed out.

    Args:
        client: aurora client object
        job_key: aurora JobKey struct specifying the job to wait for
        allowed_intermediate_statuses: a list of intermediate task state
            allowed to be in, fails immediately if any of the tasks is neither
            in intermediate state or target state
        status: target task state to wait for
        timeout_secs: timeout in seconds, triggers assertion failure if
            timed out
    '''
    deadline = time.time() + timeout_secs
    while time.time() < deadline:
        statuses = get_task_status(client, job_key)
        all_match = True
        for s in statuses:
            if s != status:
                assert s in allowed_intermediate_statuses
                all_match = False
        if all_match:
            return
        time.sleep(2)

    assert False, 'timed out waiting for {status}, last statuses: {latest}'.format(
        status=api.ScheduleStatus.name_of(status),
        latest=map(lambda s: api.ScheduleStatus.name_of(s), statuses))


def get_task_status(client, job_key):
    '''Querying current task status for job.

    Args:
        client: aurora client object
        job_key: aurora JobKey struct specifying the job to query for

    Returns:
        a list of ScheduleStatus enum representing the state for all tasks
    '''
    res = client.get_tasks_without_configs(api.TaskQuery(jobKeys=[job_key]))
    check_response_ok(res)

    tasks = res.result.scheduleStatusResult.tasks
    assert tasks is not None

    return [t.status for t in tasks]


def get_job_update_request(config_path):
    '''Load aurora JobUpdateRequest struct from yaml file.

    Args:
        config_path: path to yaml file containing JobUpdateRequest

    Returns:
        aurora JobUpdateRequest struct
    '''
    config_dump = load_config(config_path, TEST_CONFIG_DIR)
    return api.JobUpdateRequest.from_primitive(config_dump)


def start_job_update(client, config_path, update_message=''):
    '''Starts a job update and waits for the update to be in "ROLLED_FORWARD"
    state and all tasks in the job are in "RUNNING" state.

    Args:
        client: aurora client object
        config_path: path to yaml file containing JobUpdateRequest
        update_message: optional message to be passed to the update
    '''
    req = get_job_update_request(config_path)
    resp = client.start_job_update(req, update_message)
    check_response_ok(resp)
    assert resp.result.startJobUpdateResult is not None
    job_update_key = resp.result.startJobUpdateResult.key
    wait_for_rolled_forward(client, resp.result.startJobUpdateResult.key)
    wait_for_running(client, job_update_key.job)
    return job_update_key
