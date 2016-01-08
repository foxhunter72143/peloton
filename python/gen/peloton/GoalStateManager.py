#
# Autogenerated by Thrift Compiler (0.9.3)
#
# DO NOT EDIT UNLESS YOU ARE SURE THAT YOU KNOW WHAT YOU ARE DOING
#
#  options string: py:tornado,new_style,dynamic,slots,utf8strings
#

from thrift.Thrift import TType, TMessageType, TException, TApplicationException
import logging
from ttypes import *
from thrift.Thrift import TProcessor
from thrift.protocol.TBase import TBase, TExceptionBase, TTransport

from tornado import gen
from tornado import concurrent
from thrift.transport import TTransport

class Iface(object):
  """
  Goal State Service interface that will be implemented by a GoalState
  server and consumed by GoalState agents and clients.
  """
  def setGoalStates(self, states):
    """
    Set the goal states for the list of instances. Will create the
    goal state if not exists. Otherwise, the existing goal state will
    be updated ( Append only ??).

    Parameters:
     - states
    """
    pass

  def queryGoalStates(self, queries):
    """
    Query goal states for a list of service instances. Will omit the data
    field in the return GoalState if the digest matches.

    Parameters:
     - queries
    """
    pass

  def updateActualStates(self, states):
    """
    Update the actual states for the list of instances. The goal state agent
    also uses this method as a way to keep-alive service instances in the
    master if the version number and digest is the same. (Append only ??)

    Parameters:
     - states
    """
    pass

  def queryActualStates(self, queries):
    """
    Query actual states of a module for a list of service instances

    Parameters:
     - queries
    """
    pass


class Client(Iface):
  """
  Goal State Service interface that will be implemented by a GoalState
  server and consumed by GoalState agents and clients.
  """
  def __init__(self, transport, iprot_factory, oprot_factory=None):
    self._transport = transport
    self._iprot_factory = iprot_factory
    self._oprot_factory = (oprot_factory if oprot_factory is not None
                           else iprot_factory)
    self._seqid = 0
    self._reqs = {}
    self._transport.io_loop.spawn_callback(self._start_receiving)

  @gen.engine
  def _start_receiving(self):
    while True:
      try:
        frame = yield self._transport.readFrame()
      except TTransport.TTransportException as e:
        for future in self._reqs.itervalues():
          future.set_exception(e)
        self._reqs = {}
        return
      tr = TTransport.TMemoryBuffer(frame)
      iprot = self._iprot_factory.getProtocol(tr)
      (fname, mtype, rseqid) = iprot.readMessageBegin()
      future = self._reqs.pop(rseqid, None)
      if not future:
        # future has already been discarded
        continue
      method = getattr(self, 'recv_' + fname)
      try:
        result = method(iprot, mtype, rseqid)
      except Exception as e:
        future.set_exception(e)
      else:
        future.set_result(result)

  def setGoalStates(self, states):
    """
    Set the goal states for the list of instances. Will create the
    goal state if not exists. Otherwise, the existing goal state will
    be updated ( Append only ??).

    Parameters:
     - states
    """
    self._seqid += 1
    future = self._reqs[self._seqid] = concurrent.Future()
    self.send_setGoalStates(states)
    return future

  def send_setGoalStates(self, states):
    oprot = self._oprot_factory.getProtocol(self._transport)
    oprot.writeMessageBegin('setGoalStates', TMessageType.CALL, self._seqid)
    args = setGoalStates_args()
    args.states = states
    args.write(oprot)
    oprot.writeMessageEnd()
    oprot.trans.flush()

  def recv_setGoalStates(self, iprot, mtype, rseqid):
    if mtype == TMessageType.EXCEPTION:
      x = TApplicationException()
      x.read(iprot)
      iprot.readMessageEnd()
      raise x
    result = setGoalStates_result()
    result.read(iprot)
    iprot.readMessageEnd()
    if result.alreadyExists is not None:
      raise result.alreadyExists
    if result.serverError is not None:
      raise result.serverError
    return

  def queryGoalStates(self, queries):
    """
    Query goal states for a list of service instances. Will omit the data
    field in the return GoalState if the digest matches.

    Parameters:
     - queries
    """
    self._seqid += 1
    future = self._reqs[self._seqid] = concurrent.Future()
    self.send_queryGoalStates(queries)
    return future

  def send_queryGoalStates(self, queries):
    oprot = self._oprot_factory.getProtocol(self._transport)
    oprot.writeMessageBegin('queryGoalStates', TMessageType.CALL, self._seqid)
    args = queryGoalStates_args()
    args.queries = queries
    args.write(oprot)
    oprot.writeMessageEnd()
    oprot.trans.flush()

  def recv_queryGoalStates(self, iprot, mtype, rseqid):
    if mtype == TMessageType.EXCEPTION:
      x = TApplicationException()
      x.read(iprot)
      iprot.readMessageEnd()
      raise x
    result = queryGoalStates_result()
    result.read(iprot)
    iprot.readMessageEnd()
    if result.success is not None:
      return result.success
    if result.serverError is not None:
      raise result.serverError
    raise TApplicationException(TApplicationException.MISSING_RESULT, "queryGoalStates failed: unknown result")

  def updateActualStates(self, states):
    """
    Update the actual states for the list of instances. The goal state agent
    also uses this method as a way to keep-alive service instances in the
    master if the version number and digest is the same. (Append only ??)

    Parameters:
     - states
    """
    self._seqid += 1
    future = self._reqs[self._seqid] = concurrent.Future()
    self.send_updateActualStates(states)
    return future

  def send_updateActualStates(self, states):
    oprot = self._oprot_factory.getProtocol(self._transport)
    oprot.writeMessageBegin('updateActualStates', TMessageType.CALL, self._seqid)
    args = updateActualStates_args()
    args.states = states
    args.write(oprot)
    oprot.writeMessageEnd()
    oprot.trans.flush()

  def recv_updateActualStates(self, iprot, mtype, rseqid):
    if mtype == TMessageType.EXCEPTION:
      x = TApplicationException()
      x.read(iprot)
      iprot.readMessageEnd()
      raise x
    result = updateActualStates_result()
    result.read(iprot)
    iprot.readMessageEnd()
    if result.serverError is not None:
      raise result.serverError
    return

  def queryActualStates(self, queries):
    """
    Query actual states of a module for a list of service instances

    Parameters:
     - queries
    """
    self._seqid += 1
    future = self._reqs[self._seqid] = concurrent.Future()
    self.send_queryActualStates(queries)
    return future

  def send_queryActualStates(self, queries):
    oprot = self._oprot_factory.getProtocol(self._transport)
    oprot.writeMessageBegin('queryActualStates', TMessageType.CALL, self._seqid)
    args = queryActualStates_args()
    args.queries = queries
    args.write(oprot)
    oprot.writeMessageEnd()
    oprot.trans.flush()

  def recv_queryActualStates(self, iprot, mtype, rseqid):
    if mtype == TMessageType.EXCEPTION:
      x = TApplicationException()
      x.read(iprot)
      iprot.readMessageEnd()
      raise x
    result = queryActualStates_result()
    result.read(iprot)
    iprot.readMessageEnd()
    if result.success is not None:
      return result.success
    if result.serverError is not None:
      raise result.serverError
    raise TApplicationException(TApplicationException.MISSING_RESULT, "queryActualStates failed: unknown result")


class Processor(Iface, TProcessor):
  def __init__(self, handler):
    self._handler = handler
    self._processMap = {}
    self._processMap["setGoalStates"] = Processor.process_setGoalStates
    self._processMap["queryGoalStates"] = Processor.process_queryGoalStates
    self._processMap["updateActualStates"] = Processor.process_updateActualStates
    self._processMap["queryActualStates"] = Processor.process_queryActualStates

  def process(self, iprot, oprot):
    (name, type, seqid) = iprot.readMessageBegin()
    if name not in self._processMap:
      iprot.skip(TType.STRUCT)
      iprot.readMessageEnd()
      x = TApplicationException(TApplicationException.UNKNOWN_METHOD, 'Unknown function %s' % (name))
      oprot.writeMessageBegin(name, TMessageType.EXCEPTION, seqid)
      x.write(oprot)
      oprot.writeMessageEnd()
      oprot.trans.flush()
      return
    else:
      return self._processMap[name](self, seqid, iprot, oprot)

  @gen.coroutine
  def process_setGoalStates(self, seqid, iprot, oprot):
    args = setGoalStates_args()
    args.read(iprot)
    iprot.readMessageEnd()
    result = setGoalStates_result()
    try:
      yield gen.maybe_future(self._handler.setGoalStates(args.states))
    except StateAlreadyExists as alreadyExists:
      result.alreadyExists = alreadyExists
    except InternalServerError as serverError:
      result.serverError = serverError
    oprot.writeMessageBegin("setGoalStates", TMessageType.REPLY, seqid)
    result.write(oprot)
    oprot.writeMessageEnd()
    oprot.trans.flush()

  @gen.coroutine
  def process_queryGoalStates(self, seqid, iprot, oprot):
    args = queryGoalStates_args()
    args.read(iprot)
    iprot.readMessageEnd()
    result = queryGoalStates_result()
    try:
      result.success = yield gen.maybe_future(self._handler.queryGoalStates(args.queries))
    except InternalServerError as serverError:
      result.serverError = serverError
    oprot.writeMessageBegin("queryGoalStates", TMessageType.REPLY, seqid)
    result.write(oprot)
    oprot.writeMessageEnd()
    oprot.trans.flush()

  @gen.coroutine
  def process_updateActualStates(self, seqid, iprot, oprot):
    args = updateActualStates_args()
    args.read(iprot)
    iprot.readMessageEnd()
    result = updateActualStates_result()
    try:
      yield gen.maybe_future(self._handler.updateActualStates(args.states))
    except InternalServerError as serverError:
      result.serverError = serverError
    oprot.writeMessageBegin("updateActualStates", TMessageType.REPLY, seqid)
    result.write(oprot)
    oprot.writeMessageEnd()
    oprot.trans.flush()

  @gen.coroutine
  def process_queryActualStates(self, seqid, iprot, oprot):
    args = queryActualStates_args()
    args.read(iprot)
    iprot.readMessageEnd()
    result = queryActualStates_result()
    try:
      result.success = yield gen.maybe_future(self._handler.queryActualStates(args.queries))
    except InternalServerError as serverError:
      result.serverError = serverError
    oprot.writeMessageBegin("queryActualStates", TMessageType.REPLY, seqid)
    result.write(oprot)
    oprot.writeMessageEnd()
    oprot.trans.flush()


# HELPER FUNCTIONS AND STRUCTURES

class setGoalStates_args(TBase):
  """
  Attributes:
   - states
  """

  __slots__ = [ 
    'states',
   ]

  thrift_spec = (
    None, # 0
    (1, TType.LIST, 'states', (TType.STRUCT,(GoalState, GoalState.thrift_spec)), None, ), # 1
  )

  def __init__(self, states=None,):
    self.states = states

  def __hash__(self):
    value = 17
    value = (value * 31) ^ hash(self.states)
    return value


class setGoalStates_result(TBase):
  """
  Attributes:
   - alreadyExists
   - serverError
  """

  __slots__ = [ 
    'alreadyExists',
    'serverError',
   ]

  thrift_spec = (
    None, # 0
    (1, TType.STRUCT, 'alreadyExists', (StateAlreadyExists, StateAlreadyExists.thrift_spec), None, ), # 1
    (2, TType.STRUCT, 'serverError', (InternalServerError, InternalServerError.thrift_spec), None, ), # 2
  )

  def __init__(self, alreadyExists=None, serverError=None,):
    self.alreadyExists = alreadyExists
    self.serverError = serverError

  def __hash__(self):
    value = 17
    value = (value * 31) ^ hash(self.alreadyExists)
    value = (value * 31) ^ hash(self.serverError)
    return value


class queryGoalStates_args(TBase):
  """
  Attributes:
   - queries
  """

  __slots__ = [ 
    'queries',
   ]

  thrift_spec = (
    None, # 0
    (1, TType.LIST, 'queries', (TType.STRUCT,(StateQuery, StateQuery.thrift_spec)), None, ), # 1
  )

  def __init__(self, queries=None,):
    self.queries = queries

  def __hash__(self):
    value = 17
    value = (value * 31) ^ hash(self.queries)
    return value


class queryGoalStates_result(TBase):
  """
  Attributes:
   - success
   - serverError
  """

  __slots__ = [ 
    'success',
    'serverError',
   ]

  thrift_spec = (
    (0, TType.LIST, 'success', (TType.STRUCT,(GoalState, GoalState.thrift_spec)), None, ), # 0
    (1, TType.STRUCT, 'serverError', (InternalServerError, InternalServerError.thrift_spec), None, ), # 1
  )

  def __init__(self, success=None, serverError=None,):
    self.success = success
    self.serverError = serverError

  def __hash__(self):
    value = 17
    value = (value * 31) ^ hash(self.success)
    value = (value * 31) ^ hash(self.serverError)
    return value


class updateActualStates_args(TBase):
  """
  Attributes:
   - states
  """

  __slots__ = [ 
    'states',
   ]

  thrift_spec = (
    None, # 0
    (1, TType.LIST, 'states', (TType.STRUCT,(ActualState, ActualState.thrift_spec)), None, ), # 1
  )

  def __init__(self, states=None,):
    self.states = states

  def __hash__(self):
    value = 17
    value = (value * 31) ^ hash(self.states)
    return value


class updateActualStates_result(TBase):
  """
  Attributes:
   - serverError
  """

  __slots__ = [ 
    'serverError',
   ]

  thrift_spec = (
    None, # 0
    (1, TType.STRUCT, 'serverError', (InternalServerError, InternalServerError.thrift_spec), None, ), # 1
  )

  def __init__(self, serverError=None,):
    self.serverError = serverError

  def __hash__(self):
    value = 17
    value = (value * 31) ^ hash(self.serverError)
    return value


class queryActualStates_args(TBase):
  """
  Attributes:
   - queries
  """

  __slots__ = [ 
    'queries',
   ]

  thrift_spec = (
    None, # 0
    (1, TType.LIST, 'queries', (TType.STRUCT,(StateQuery, StateQuery.thrift_spec)), None, ), # 1
  )

  def __init__(self, queries=None,):
    self.queries = queries

  def __hash__(self):
    value = 17
    value = (value * 31) ^ hash(self.queries)
    return value


class queryActualStates_result(TBase):
  """
  Attributes:
   - success
   - serverError
  """

  __slots__ = [ 
    'success',
    'serverError',
   ]

  thrift_spec = (
    (0, TType.LIST, 'success', (TType.STRUCT,(ActualState, ActualState.thrift_spec)), None, ), # 0
    (1, TType.STRUCT, 'serverError', (InternalServerError, InternalServerError.thrift_spec), None, ), # 1
  )

  def __init__(self, success=None, serverError=None,):
    self.success = success
    self.serverError = serverError

  def __hash__(self):
    value = 17
    value = (value * 31) ^ hash(self.success)
    value = (value * 31) ^ hash(self.serverError)
    return value

